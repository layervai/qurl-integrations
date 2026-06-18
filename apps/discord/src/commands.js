const {
  SlashCommandBuilder,
  EmbedBuilder,
  PermissionFlagsBits,
  ActionRowBuilder,
  ButtonBuilder,
  ButtonStyle,
  ComponentType,
  StringSelectMenuBuilder,
  UserSelectMenuBuilder,
  MentionableSelectMenuBuilder,
  ModalBuilder,
  TextInputBuilder,
  TextInputStyle,
  AttachmentBuilder,
} = require('discord.js');
const { Routes } = require('discord-api-types/v10');
const crypto = require('crypto');
const config = require('./config');
const db = require('./store');
const logger = require('./logger');
const viewUpdateRegistry = require('./view-update-registry');
const { createHandleViewUpdate } = require('./view-update-handler');
const { COLORS, TIMEOUTS, RESOURCE_TYPES, DM_STATUS, MAX_FILE_SIZE, MAX_CONCURRENT_MONITORS, DISCORD_MEMBERS_PAGE_SIZE, PREWARM_MAX_PAGES, UNLINKED_CACHE_COMPLETENESS_THRESHOLD, AUDIT_EVENTS, TRUST } = require('./constants');
const {
  expiryToISO,
  expiryToMs,
  formatSelfDestructLabel,
  formatSelfDestructSegment,
  isPositiveFinite,
  selfDestructSelectValueToSeconds,
  isLegitimateSelfDestructSelectValue,
  SELF_DESTRUCT_PRESETS,
  SELF_DESTRUCT_NO_TIMER_VALUE,
} = require('./utils/time');
const { requireAdmin } = require('./utils/admin');
const { signQurlOAuthState } = require('./utils/qurl-oauth-state');
const { deleteLink } = require('./qurl');
const { downloadAndUpload, reUploadBuffer, mintLinks, detectWatermark, uploadJsonToConnector, isAllowedSourceUrl } = require('./connector');
const { deleteFlow, transitionFlow, supersedeOrCreate } = require('./flow-state');
const { fireAndForgetLinkGuildWebhookSubscription } = require('./guild-webhook-link');
const { flowIdForInteraction, registerFlow, safeReply, siblingMessageForStage } = require('./flow-dispatch');
const {
  searchPlaces,
  findPlaceFromText,
  getPlaceDetails,
  buildPlaceUrl,
  encodePlaceIdSentinel,
  decodePlaceIdSentinel,
  PLACE_ID_SHAPE_RE,
} = require('./places');

// Max tokens the QURL API allows per resource. When exceeded, a new
// resource must be created (re-upload) to get a fresh token pool.
const TOKENS_PER_RESOURCE = 10;
// Process-local connector capability cache. Start optimistic so legacy/prod
// connectors keep batched mints; once this worker observes the meta-seal
// cap-1 response, later sends avoid burning one known-failing batch probe.
// The ratchet resets only on worker restart; n=1 stays correct if a connector
// is reconfigured back to legacy before the bot is redeployed.
let connectorMintLinksPerRequest = TOKENS_PER_RESOURCE;

// Absolute floor above which a single send earns a `WARN`-level
// audit log at executeSendPipeline entry. 1000 chosen as the cliff
// where DM fan-out at Discord's ~5/sec per-bot limit starts taking
// minutes (1000 / 5 = ~3 min) and qurl-service re-uploads get
// non-trivial (1000 / TOKENS_PER_RESOURCE = 100 re-uploads).
//
// The effective threshold (`largeSendThreshold()` below) takes the
// MIN of this floor and half the configured cap, so operators who
// env-override `QURL_SEND_MAX_RECIPIENTS` down (e.g., 500) still see
// the WARN fire on sends that are "operationally large for them"
// (250 on a 500 cap) rather than only on sends near the absolute
// floor. Sub-threshold sends stay quiet at the default `INFO` level
// so log volume tracks operational importance.
const LARGE_SEND_RECIPIENT_FLOOR = 1000;
function largeSendThreshold() {
  // `half || 1` floors the threshold at 1 so a pathologically-low cap
  // override (e.g., cap=1 → floor(0.5)=0) doesn't make `>= 0` always
  // true and fire the WARN on every send. Threshold is always a
  // positive integer; the config validator rejects cap ≤ 0 so the
  // 0→1 substitution only fires for cap=1.
  const half = Math.floor(config.QURL_SEND_MAX_RECIPIENTS / 2);
  return Math.min(LARGE_SEND_RECIPIENT_FLOOR, half || 1);
}

// classifyMintFailure maps a thrown error from the upload + mint phase
// of /qurl send into a small enum of reason strings. Used as the
// `reason` field on the QURL_SEND_CREATE_LINK_FAILURE audit event
// (qurl-integrations#276) so the CloudWatch metric filter can split
// failures by class — 4xx (likely client/contract problem) vs 5xx
// (likely upstream outage) vs timeout (likely network/cold-start).
//
// Don't add per-API-code branches here — that explodes the metric
// cardinality. New reasons should map to one of the existing
// categories or earn a new top-level category with a clear
// dimensional purpose (e.g. a future "rate_limit" if we add upstream
// 429 handling).
function classifyMintFailure(error) {
  if (!error) return 'unknown';
  // AbortError is ambiguous: undici/fetch raises it on deadline-fired
  // aborts AND on user-cancellations. error.cause is the only reliable
  // disambiguator — bucket as `timeout` only when cause corroborates a
  // timeout signal; otherwise fall through to `unknown` so a future
  // cancel-button adoption doesn't silently mis-bucket cancellations
  // as timeouts.
  if (error.name === 'AbortError') {
    const causeStr = String((error.cause && (error.cause.message || error.cause)) || '');
    // Word-anchored to drop strange concatenations like "rtimeout" or
    // "timeouted" — but it does NOT defend against "not due to timeout"
    // because "timeout" is still a complete word in that string. The
    // false-positive class for negation-containing causes is acceptable
    // because real timeout-driven aborts in undici raise TimeoutError
    // (handled by the name/code branch below) rather than AbortError; this
    // branch defends against libraries that use controller.abort('timeout')
    // as their deadline signal, where the cause is the deliberate marker.
    // This returns BEFORE the status check below — an abort's error.status
    // (if it somehow carried one) is not meaningful, so abort disambiguation
    // deliberately takes priority over status classification.
    if (/\btimed?\s*out\b/i.test(causeStr)) return 'timeout';
    return 'unknown';
  }
  // libuv socket codes + undici/fetch TimeoutError DOMException — all
  // unambiguous deadline-shaped failures. The message-regex branch was
  // removed: `/timeout/i.test(error.message)` over-matched
  // any string containing the substring "timeout" (e.g. "not a timeout
  // related error") and a future upstream message like "completed
  // after timeout retry" would mis-bucket. The name/code paths above
  // cover every real shape we've seen.
  if (error.code === 'ETIMEDOUT' || error.code === 'ECONNABORTED' ||
      error.name === 'TimeoutError') {
    return 'timeout';
  }
  const status = error.status ?? 0;
  if (status >= 500 && status < 600) return 'upstream_5xx';
  if (status >= 400 && status < 500) return 'upstream_4xx';
  return 'unknown';
}

// emitMintFailureAudit centralizes the QURL_SEND_CREATE_LINK_FAILURE
// emission contract so the three catch sites (initial /qurl send +
// addRecipients file branch + addRecipients location branch) cannot
// drift on the skip rule or field shape. Adding a fourth call site
// goes through here too — owns both the contract and the cardinality
// discipline in one place. See #276.
function emitMintFailureAudit(error, { sendId, kind }) {
  if (error && error.apiCode === 'quota_exceeded') return;
  logger.audit(AUDIT_EVENTS.QURL_SEND_CREATE_LINK_FAILURE, {
    send_id: sendId,
    reason: classifyMintFailure(error),
    // `?? null` (not `|| null`) preserves an empty-string `apiCode` —
    // `|| null` would collapse it to null and lose a forensic dimension.
    // (Status 0 doesn't apply: native fetch throws on transport errors
    // rather than returning a 0-status response.)
    api_code: error?.apiCode ?? null,
    status_code: error?.status ?? null,
    kind,
  });
}

// Shared helper: many Discord API calls (edits, updates, follow-ups) are
// best-effort — if the interaction token expired or Discord is briefly
// degraded, we log a warning and continue rather than fail the whole flow.
// Extracted to deduplicate ~13 identical `.catch(err => logger.warn(...))`
// one-liners across this file.
const logIgnoredDiscordErr = (err) => logger.warn('Discord API op failed (ignored)', { error: err.message });
const { sendDM } = require('./discord');
const { editDM, sendChannelMessage } = require('./discord-rest');
// renderViewCounter lives in its own leaf module so the cross-replica
// webhook fast-path (routes/qurl-webhook.js) can import the SAME pure
// renderer without pulling commands.js (+ discord.js) into the HTTP
// receiver's require graph. The monitor's buildStatusMsg() below + the
// fast-path both call it, so the confirmation body stays byte-identical
// across replicas. Still re-exported via _test (see module.exports) so
// the byte-identity unit test reads it where it always did.
const { renderViewCounter } = require('./view-counter-render');


// Generate an OAuth state token bound to the initiating Discord user.
//
// Format: `{nonce}.{hmac}` where hmac = HMAC-SHA256(OAUTH_STATE_SECRET,
// `${discordId}:${nonce}`). On callback we re-compute the HMAC against the
// discord_id pulled from consumePendingLink(); a mismatch means the state
// was tampered with or replayed across users, even if the random nonce
// happened to collide with a live pending row.
//
// Defense-in-depth only — the primary binding is the single-use DB row
// plus the HttpOnly/SameSite=Lax session cookie. This adds a third check
// so a stolen state URL cannot be silently coerced to another user.
let _warnedStateSecretFallback = false;
// Random per-process fallback so even inside the Jest harness there's no
// static key that, if accidentally shipped, would be forgeable. Regenerated
// on every process start; tests that need a stable secret should set
// OAUTH_STATE_SECRET explicitly in their own mocks.
const _testFallbackSecret = crypto.randomBytes(32).toString('hex');
function stateSecret() {
  // Prefer a dedicated OAUTH_STATE_SECRET so a compromised GITHUB_CLIENT_SECRET
  // can be rotated without also invalidating in-flight OAuth state tokens —
  // and vice versa. Blast-radius isolation: leaking one doesn't enable
  // forgery of the other's use cases. Fall back to GITHUB_CLIENT_SECRET for
  // backward-compat with existing deployments.
  const dedicated = process.env.OAUTH_STATE_SECRET;
  if (dedicated) return dedicated;
  if (!config.GITHUB_CLIENT_SECRET) {
    // Only use the static fallback inside Jest (NODE_ENV=test AND either
    // JEST_WORKER_ID set by Jest, or CI=true). This raises the bar: merely
    // setting NODE_ENV=test by accident in a deployed env doesn't enable
    // the forgeable key. Everywhere else throws hard so a misconfig is loud.
    const inTestHarness = process.env.NODE_ENV === 'test'
      && (process.env.JEST_WORKER_ID || process.env.CI === 'true');
    if (!inTestHarness) {
      throw new Error('Refusing to mint OAuth state: OAUTH_STATE_SECRET or GITHUB_CLIENT_SECRET must be set.');
    }
    if (!_warnedStateSecretFallback) {
      logger.warn('OAuth state HMAC using per-process random test fallback — set OAUTH_STATE_SECRET or GITHUB_CLIENT_SECRET');
      _warnedStateSecretFallback = true;
    }
    return _testFallbackSecret;
  }
  return config.GITHUB_CLIENT_SECRET;
}
function generateState(discordId) {
  const nonce = crypto.randomBytes(16).toString('hex');
  const sig = crypto.createHmac('sha256', stateSecret())
    .update(`${discordId}:${nonce}`)
    .digest('hex');
  return `${nonce}.${sig}`;
}
function verifyStateBinding(state, discordId) {
  if (typeof state !== 'string') return false;
  const parts = state.split('.');
  if (parts.length !== 2) return false;
  const [nonce, sig] = parts;
  if (!/^[0-9a-f]{32}$/.test(nonce) || !/^[0-9a-f]{64}$/.test(sig)) return false;
  const expected = crypto.createHmac('sha256', stateSecret())
    .update(`${discordId}:${nonce}`)
    .digest('hex');
  try {
    return crypto.timingSafeEqual(Buffer.from(sig, 'hex'), Buffer.from(expected, 'hex'));
  } catch { return false; }
}

// --- QURL send helpers ---

function isGoogleMapsURL(url) {
  try {
    const parsed = new URL(url);
    if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') return false;
    const host = parsed.hostname.toLowerCase();
    // Match google.com, google.co.uk, google.com.au but not google.evil.com
    const isGoogleHost = /^(www\.)?google\.[a-z]{2,3}(\.[a-z]{2})?$/.test(host) ||
                         /^maps\.google\.[a-z]{2,3}(\.[a-z]{2})?$/.test(host) ||
                         host === 'goo.gl' ||
                         host === 'maps.app.goo.gl';
    if (!isGoogleHost) return false;
    if (host === 'goo.gl') return parsed.pathname.startsWith('/maps/');
    if (host === 'maps.app.goo.gl') return true;
    if (host.startsWith('maps.google.')) return true;
    return parsed.pathname.startsWith('/maps');
  } catch {
    return false;
  }
}



const { DISPLAY_NAME_FALLBACK, sanitizeFilename, escapeDiscordMarkdown, sanitizeDisplayName, sanitizeDisplayNamePlain, sanitizeContentLabel, stripBidiAndControls } = require('./utils/sanitize');

// Best-effort host extraction for log lines. URL parsing throws on
// pathological input (no scheme, embedded null, etc.) — swallow and
// return a marker so a log line is still useful in triage.
function safeUrlHost(url) {
  try { return new URL(url).host; } catch { return 'invalid-url'; }
}

/**
 * Codepoint-aware truncation with an odd-backslash backoff. Two
 * concerns this guards against:
 *  1. Surrogate split — bare String.prototype.slice operates on UTF-16
 *     code units, so cutting at a boundary that lands inside a
 *     surrogate pair (e.g. \u{1F600}) emits a lone high surrogate
 *     that Discord renders as tofu.
 *  2. Dangling escape backslash — a markdown-escaped input may have
 *     `\X` sequences. A slice that lands ON the `\` separates the
 *     escape from its target. Count trailing `\` chars; odd count →
 *     back off by 1 so the escape stays intact.
 *
 * Returns the truncated string (no ellipsis appended — callers add
 * their own truncation indicator if desired).
 *
 * Edge case: if `cut === 1` and the only char before it is `\`, the
 * backoff sets `cut = 0` and the slice returns `''`. Safe failure
 * mode for the 500-codepoint cap (callers don't depend on the result
 * being non-empty), but worth knowing if you wire this into a smaller
 * cap or a UI surface that distinguishes empty from truncated.
 */
function safeCodepointSlice(s, maxCodepoints) {
  // Fast path: UTF-16 code-unit count is an upper bound on codepoint
  // count (every codepoint is 1 or 2 code units), so a string whose
  // `.length` is below the cap CANNOT exceed the cap in codepoints.
  // Skips the `Array.from` allocation on the common case where the
  // input is already well under the cap (the confirm-card render
  // path hits this branch for every typical-sized message).
  if (s.length <= maxCodepoints) return s;
  const codepoints = Array.from(s);
  if (codepoints.length <= maxCodepoints) return s;
  let cut = maxCodepoints;
  let bs = 0;
  for (let i = cut - 1; i >= 0 && codepoints[i] === '\\'; i--) bs++;
  if (bs % 2 === 1) cut -= 1;
  return codepoints.slice(0, cut).join('');
}

/**
 * Cap `s` to `maxCodepoints` codepoints and append `indicator` only
 * when truncation actually occurred. Relies on `safeCodepointSlice`'s
 * "return the input unchanged when below the cap" contract — reference
 * equality (`=== s`) is the truncation sentinel, so both callers
 * (`renderRecipientWarnings` per-token, `renderConfirmCardContent`
 * total) can use the same shape without a separate length probe.
 *
 * Final output is at most `maxCodepoints + indicator.length` codepoints.
 */
function sliceWithEllipsis(s, maxCodepoints, indicator) {
  const sliced = safeCodepointSlice(s, maxCodepoints);
  return sliced === s ? s : sliced + indicator;
}

// Discord-enforced input bound on every legitimate personalMessage
// ingress (slash option setMaxLength + modal TextInput setMaxLength).
// `personalMessageRaw` storage uses this cap so forged interactions
// can't grow flow rows past the legitimate ceiling.
const PERSONAL_MESSAGE_INPUT_MAX = 280;

// Triggers `guild.members.fetch()` pre-warm before parsing recipients —
// any `@everyone` text token OR any `<@&{snowflake}>` role-mention. Both
// paths read `guild.members.cache`, which discord.js v14 leaves empty
// (we deliberately skip `chunkOnStartup`/`ws.large_threshold` on the
// multi-tenant gateway — eagerly fetching every member of every joined
// guild on connect would blow the rate-limit budget at install spikes).
// Shape-only — permission gating happens later inside the parser via
// `allowMassMention` / `role.mentionable`. Word-boundary class is a
// parallel copy of recipient-parser.js's `EVERYONE_TOKEN_RE` so
// `@everyonefoo` doesn't false-trigger the fetch (drift between the
// two would silently cache-warm for inputs the parser then ignores).
// If `EVERYONE_TOKEN_RE` ever moves to a shared module, derive this
// from it rather than maintaining the parallel copy.
const MASS_MENTION_HINT_RE = /(?<![\p{L}\p{N}_])@everyone(?![\p{L}\p{N}_])|<@&\d+>/u;
const ROLE_MENTION_HINT_RE = /<@&\d+>/u;

// discord.js `Guild._patch` only sets `memberCount` from the gateway
// GUILD_CREATE payload. The REST `GET /guilds/:id?with_counts=true`
// path populates `approximateMemberCount` instead. HTTP-only worker
// mode never sees GUILD_CREATE, so `memberCount` stays undefined —
// without this fallback every @everyone render and prewarm gate hits
// the wrong branch in that tier.
function effectiveGuildMemberCount(guild) {
  return guild?.memberCount ?? guild?.approximateMemberCount;
}

// Helper for the two pre-warm sites. Returns a Promise so the caller
// can await before reading `guild.members.cache`. Swallows errors —
// degraded expansion (parser sees a partial cache) is the correct
// fallback for transient API blips; surfacing the failure to the user
// would confuse them about a problem they can't act on. Hot-cache
// short-circuit fires ONLY when `guild.memberCount` is present and
// matched — see the inline comment at the gate for why
// `approximateMemberCount` is deliberately not consulted here. The
// populated cache is intentionally retained — discord.js `Collection`
// has no LRU and eviction relies on `GUILD_MEMBER_REMOVE` gateway
// events. Acceptable for our scale; revisit if a future regression in
// gateway event delivery causes drift.
//
// In-flight de-duplication via the `prewarmInFlight` map below: two
// concurrent `/qurl send @everyone` invocations in the same guild from
// a cold cache share the same underlying pagination promise. Without
// coalescing, abuse via concurrent invocations could burn REST budget
// linearly. The map is keyed by `guild.id` so cross-guild calls still
// proceed in parallel.
const prewarmInFlight = new Map();
// `logCtx` is spread first in every log payload below so explicit
// fields after it (`guild_id`, `cache_size`, etc.) override any
// matching key in the caller's `logCtx`. Don't put `guild_id` in
// `logCtx` at call sites expecting it to win — it won't.
async function prewarmGuildMembersCache(guild, logCtx) {
  // Uniform Promise return shape regardless of which branch the caller
  // hits — early-exit paths used to resolve `undefined`, which works
  // for `await prewarmGuildMembersCache(...)` callers but would surprise
  // a future caller that does `.then(...)`.
  if (!guild || !guild.members || typeof guild.members.list !== 'function') return;
  const cacheSize = guild.members.cache?.size ?? 0;
  // Only short-circuit on EXACT `memberCount` (gateway tier). The
  // `approximateMemberCount` value can underreport by hundreds — if the
  // cache happened to be partially warmed by per-user `members.fetch(id)`
  // calls from `resolveRecipientUsers`, an approximate-based gate could
  // fire prematurely and underresolve @everyone, re-introducing this
  // PR's original bug in a quieter form. http-only tier (no
  // `memberCount`) accepts the perf cost of always paginating; the
  // in-flight dedup below prevents concurrent invocations from stacking.
  if (guild.memberCount != null && cacheSize >= guild.memberCount) {
    // Observability hook for the hot-cache short-circuit firing. Without
    // this, dashboards can't correlate "how often is the cache warm" with
    // memory growth on large-guild workers.
    logger.debug('members pre-warm short-circuited (hot cache)', {
      ...logCtx, guild_id: guild.id, cache_size: cacheSize, member_count: guild.memberCount,
    });
    return;
  }
  const existing = prewarmInFlight.get(guild.id);
  if (existing) return existing;
  const inFlight = (async () => {
    try {
      // REST `GET /guilds/{id}/members` rather than `guild.members.fetch()`
      // (no args): the latter sends REQUEST_GUILD_MEMBERS over the gateway
      // WebSocket via `shard.send`, which crashes in http-only worker mode
      // (no gateway shard). discord.js handles 429 backoff inside `list`
      // so we don't sleep here.
      //
      // No wall-clock deadline on the loop: @everyone must mean EVERYONE,
      // and a wall-clock cap would silently truncate large-guild expansion
      // back to the bug this PR fixes (just with a different failure
      // mode). The reply is deferred so Discord's 3s rule doesn't apply;
      // user-perceived latency on huge guilds is the accepted trade-off.
      let after;
      // `page` is declared outside the loop because the post-loop
      // safety-cap check (`page === PREWARM_MAX_PAGES`) needs to read
      // its final value. A future "scope this inside the for" cleanup
      // would silently disable the safety-cap warn.
      let page;
      // `bailed` distinguishes "loop exited cleanly on a partial page"
      // (success) from "loop bailed on an upstream-bug guard". Without
      // it, both bail paths would also emit the success info-log below.
      let bailed = false;
      for (page = 0; page < PREWARM_MAX_PAGES; page++) {
        // `cache: true` is the discord.js default, but pin it
        // explicitly: a future major-version flip of the default
        // would silently re-degrade @everyone expansion to the bug
        // this PR fixes, and the cache-merge regression test can't
        // catch a class of failure where the mock differs from prod.
        const batch = await guild.members.list({ limit: DISCORD_MEMBERS_PAGE_SIZE, after, cache: true });
        if (batch.size < DISCORD_MEMBERS_PAGE_SIZE) break;
        // Collection is insertion-ordered against the API response, which
        // Discord sorts ascending by user_id — lastKey() is the right
        // `after` cursor for the next page.
        const nextAfter = batch.lastKey();
        // Defensive: a non-empty Collection's `lastKey()` always returns
        // a real key today, but `nextAfter == null` would equal an
        // undefined `after` on iteration 0 and false-negative the
        // non-advancement guard below.
        if (nextAfter == null) {
          logger.warn('members pre-warm received full page with null cursor; bailing', {
            ...logCtx, guild_id: guild.id, pages: page + 1,
          });
          bailed = true;
          break;
        }
        // If Discord ever returns a full page without advancing the
        // cursor (upstream bug), bail rather than burn 1000 iterations
        // re-fetching the same window forever.
        if (nextAfter === after) {
          logger.warn('members pre-warm cursor did not advance; bailing', {
            ...logCtx, guild_id: guild.id, after, pages: page + 1,
          });
          bailed = true;
          break;
        }
        after = nextAfter;
      }
      if (page === PREWARM_MAX_PAGES) {
        // `pages: PREWARM_MAX_PAGES` (not `page`) for read-clarity —
        // they're numerically equal here but the constant makes the
        // intent obvious vs. the bail/null-cursor warns which use
        // `page + 1` (the count of completed list() calls at the
        // point of the break).
        logger.warn('members pre-warm hit safety cap; @everyone/role expansion may underresolve', {
          ...logCtx, guild_id: guild.id, pages: PREWARM_MAX_PAGES, cache_size: guild.members.cache?.size,
        });
      } else if (!bailed) {
        // Successful-completion observability. Captures cache footprint
        // for large-guild memory tracking — paginating a 500k-member
        // guild persists 500k GuildMember objects in-process until the
        // worker recycles. Logged at `debug` (not `info`) because high-
        // traffic http-only workers may fire this thousands of times
        // per day; warn paths above remain at `warn` so the degraded
        // signal isn't drowned out.
        logger.debug('members pre-warm complete', {
          ...logCtx, guild_id: guild.id, pages: page + 1, cache_size: guild.members.cache?.size,
        });
      }
    } catch (err) {
      logger.warn('members pre-warm failed; @everyone/role expansion may underresolve', {
        ...logCtx, guild_id: guild.id, error: err && err.message,
      });
    } finally {
      prewarmInFlight.delete(guild.id);
    }
  })();
  prewarmInFlight.set(guild.id, inFlight);
  return inFlight;
}

function sanitizeMessage(msg) {
  // Order matters:
  //  1. NFKC-normalize + strip bidi/zero-width/control codepoints first
  //     (stripBidiAndControls) \u2014 defends against U+202E RLO spoofing in
  //     the recipient's DM body AND the sender's confirm-card preview.
  //     Without this, a crafted personal-message containing U+202E
  //     flips text direction in the rendered embed/blockquote.
  //  2. Strip @-mention abuse next (the closing `>` of `<@123>` would
  //     otherwise be escaped by step 3 and the mention regex wouldn't
  //     match).
  //  3. Escape Discord markdown so a crafted message like
  //     `[Free Prizes](https://phishing.com)` can't render as a masked
  //     link.
  // The `[mention]` literal is re-applied post-escape as a plain
  // substitution so the brackets stay visible to the user.
  // Sentinel survives the markdown-escape pass unchanged: it contains
  // no chars in the escape regex ([\*~`>|\[\]()\\_]). Suffix/prefix of
  // random hex so it can't collide with anything a user would
  // plausibly type.
  const MENTION_SENTINEL = 'XMENTIONX74caf3b0e79aXMENTIONX';
  const stripped = stripBidiAndControls(msg)
    .replace(/@(everyone|here)/gi, '@\u200b$1')
    .replace(/<@[!&]?\d+>/g, MENTION_SENTINEL);
  const escaped = escapeDiscordMarkdown(stripped)
    .replaceAll(MENTION_SENTINEL, '[mention]');
  // safeCodepointSlice: codepoint-aware + odd-backslash backoff. Bare
  // String.prototype.slice could split a surrogate pair (emoji) at the
  // 500 boundary or leave a dangling `\` from a mid-cut markdown
  // escape. The slash option's setMaxLength(280) usually keeps us
  // well under 500, but this is defense-in-depth.
  return safeCodepointSlice(escaped, 500);
}

const ALLOWED_FILE_TYPES = [
  'image/', 'application/pdf', 'video/', 'audio/',
  'text/plain', 'text/csv',
  'application/zip', 'application/x-zip-compressed',
  'application/vnd.openxmlformats',
  'application/vnd.ms-',
  'application/msword',
];

// Macro-enabled Office variants (.docm, .xlsm, .pptm, etc.) are in the
// openxmlformats family but can execute VBA macros on the recipient's
// machine. Excluded even though the prefix matches. If you ever need to
// support these, require explicit user confirmation and mark the file
// as "executable content" in the DM embed.
const DENY_MIME_SUBSTRINGS = ['macroenabled', 'macro-enabled'];

// Bound concurrent in-flight file sends to prevent N × 25MB = high memory
// pressure under burst. Each send holds its attachment buffer through the
// mint-batches re-upload cycle; cap here rather than trust users to
// self-throttle. Over-cap sends get a user-facing "try again" rather than
// exhausting the process.
const MAX_CONCURRENT_FILE_SENDS = 5;
let activeFileSends = 0;
function isAllowedFileType(contentType) {
  if (!contentType) return false;
  const ct = contentType.toLowerCase();
  if (DENY_MIME_SUBSTRINGS.some(s => ct.includes(s))) return false;
  return ALLOWED_FILE_TYPES.some(prefix => ct.startsWith(prefix));
}

// Global per-sendId mutex for the "Add Recipients" flow. Node is
// single-threaded, but the handler awaits `userSelect` prompt + DDB
// writes, so a second "Add Recipients" button click on the same sendId
// can interleave between await points within the same collector
// instance (see the `addRecipientsLocks.has(sendId)` check-then-claim
// branch inside the `qurl_add_${sendId}` button handler in
// executeSendPipeline). The sync check-then-add at entry plus
// release-in-finally is the single source of truth for "an
// addRecipients pass is in progress." Also intentional belt-and-
// suspenders against a future refactor that shares a sendId across
// collector instances (e.g. flow_state RESUME on a bot restart loading
// unfinished sends).
const addRecipientsLocks = new Set();
// Same-process per-send Revoke lock. Collector-local `revokeInFlight`
// handles duplicate clicks inside one management collector; this Set lets
// another collector in the same process see a Revoke already mutating the
// send. Cross-process safety still relies on revoked_at/#862.
const revokingSendLocks = new Set();
const ADD_RECIPIENTS_IN_PROGRESS_MSG = 'Already processing an "Add Recipients" action.';
const ALREADY_REVOKING_SEND_MSG = 'Already revoking links for this send.';

const sendCooldowns = new Map();

// Hard ceiling so a bad actor spraying unique user IDs (or a bug generating
// them) can't grow a cooldown Map beyond this. Above this, the 10%-drop
// eviction still fires; if that fails to reclaim, evictOldest drops the
// oldest single entries to guarantee we never exceed the cap. Shared by
// sendCooldowns and detectCooldowns so the two buckets stay byte-for-byte
// parallel (see evictOldest).
const COOLDOWNS_MAX = 20000;

// Shared eviction for the cooldown Maps — extracted so setCooldown and
// setDetectCooldown share ONE implementation rather than two parallel
// copies that could silently drift. Two phases, both relative to `max`:
//   1. Soft bulk-drop: once the Map crosses HALF the cap, evict the oldest
//      ~10% in one pass (cheap amortized trim so we rarely reach the cap).
//   2. Hard cap: drop the oldest one-at-a-time until size <= max, so the
//      ceiling holds even under a pathological insertion pattern that beats
//      the bulk drop.
// Relies on Map insertion-order iteration (`.keys()` yields oldest first);
// callers delete+re-set on touch so active entries re-insert at the tail.
function evictOldest(map, max) {
  const softThreshold = Math.floor(max / 2);
  if (map.size > softThreshold) {
    const dropCount = Math.max(1, Math.floor(map.size / 10));
    const it = map.keys();
    for (let i = 0; i < dropCount; i++) {
      const k = it.next().value;
      if (k === undefined) break;
      map.delete(k);
    }
  }
  while (map.size > max) {
    const oldest = map.keys().next().value;
    if (oldest === undefined) break;
    map.delete(oldest);
  }
}

function isOnCooldown(userId) {
  const last = sendCooldowns.get(userId);
  if (!last) return false;
  return Date.now() - last < config.QURL_SEND_COOLDOWN_MS;
}

function setCooldown(userId) {
  // LRU-ish behavior: delete first so set() re-inserts at the end of the
  // Map's insertion order. Combined with evictOldest's bulk 10%-drop,
  // active users stay resident while stale entries roll out.
  sendCooldowns.delete(userId);
  sendCooldowns.set(userId, Date.now());
  evictOldest(sendCooldowns, COOLDOWNS_MAX);
}

function clearCooldown(userId) {
  sendCooldowns.delete(userId);
}

// ── /qurl detect cooldown (#1101) ──
//
// AUTHORITATIVE abuse enforcement for the deanonymization oracle lives on
// the CONNECTOR: it guild-scopes every lookup via the X-Guild-Id header and
// rate-limits per guild (a 429 we honor by KEEPING this cooldown to back
// off). This in-process Map is best-effort DEFENSE-IN-DEPTH — it's not
// shared across shards/instances, so a multi-instance deploy could let a
// determined caller exceed the per-process budget. It still dampens the
// common single-instance abuse case and gives the connector room to apply
// the real limit.
//
// SEPARATE Map from sendCooldowns, and keyed on a COMPOSITE `${guildId}:
// ${userId}` rather than userId alone. Two deliberate differences from the
// send bucket:
//   1. Distinct bucket — a user's detect throttle must not lock out their
//      /qurl send (and vice-versa); these are unrelated rate limits.
//   2. Per-(guild,user) — /qurl detect attribution is guild-scoped, so the
//      bot-side throttle is too. The same user probing two guilds gets two
//      independent budgets; a shared userId key would let one guild's
//      activity throttle another's.
//   3. Separate WINDOW — keyed on config.QURL_DETECT_COOLDOWN_MS, which
//      DEFAULTS to QURL_SEND_COOLDOWN_MS (no behavior change) but is
//      independently tunable. Detect (a deanonymization oracle) and send
//      are unrelated operations; coupling their windows would let a future
//      send-cadence change silently re-tune the oracle throttle.
// Eviction/cap structure mirrors sendCooldowns: it shares the SAME
// evictOldest helper + COOLDOWNS_MAX cap + the 60s time-sweep, so the Map
// can't grow unbounded under a snowflake-spraying attacker and the two
// buckets can't drift.
const detectCooldowns = new Map();

function detectCooldownKey(guildId, userId) {
  return `${guildId}:${userId}`;
}

function isOnDetectCooldown(guildId, userId) {
  const last = detectCooldowns.get(detectCooldownKey(guildId, userId));
  if (!last) return false;
  return Date.now() - last < config.QURL_DETECT_COOLDOWN_MS;
}

function setDetectCooldown(guildId, userId) {
  const key = detectCooldownKey(guildId, userId);
  detectCooldowns.delete(key);
  detectCooldowns.set(key, Date.now());
  evictOldest(detectCooldowns, COOLDOWNS_MAX);
}

function clearDetectCooldown(guildId, userId) {
  detectCooldowns.delete(detectCooldownKey(guildId, userId));
}

async function batchSettled(items, fn, batchSize = 5) {
  const results = [];
  for (let i = 0; i < items.length; i += batchSize) {
    const batch = items.slice(i, i + batchSize);
    const batchResults = await Promise.allSettled(batch.map(fn));
    results.push(...batchResults);
  }
  return results;
}

// Resolve the sender's display alias from an interaction, the same way
// Discord itself does for any rendered name in a guild. Order:
//   1. member.displayName — guild nickname > globalName > username
//      (discord.js already resolves the inner three; we only fall
//      through if `member` is null, e.g. user-app DM invocation).
//   2. user.displayName — globalName > username (also resolved by
//      discord.js v14.18+).
//   3. user.username — direct username read; defends against test
//      mocks or shapes where the v14 displayName getter is absent.
//   4. DISPLAY_NAME_FALLBACK — last-resort so callers never get null.
// Optional chains throughout so a malformed interaction (no user, no
// member) returns the fallback instead of throwing inside DM-dispatch.
// Used by the DM-embed renderer so every recipient sees the same
// sender name across the dispatch.
function resolveSenderAlias(interaction) {
  return interaction?.member?.displayName
    ?? interaction?.user?.displayName
    ?? interaction?.user?.username
    ?? DISPLAY_NAME_FALLBACK;
}

// Author-row provenance fields for the per-recipient DM. The
// `interaction?.guild?` chain defends the edge case where guild is
// null (commands are guild-only so this shouldn't happen in
// production). `iconURL()` returns `string | null`; coalesce to
// `undefined` because some discord.js versions stringify null into
// the iconURL slot.
//
// `size: 64` matches Discord's embed-author iconURL rendering
// (~24×24px on most clients, 2× retina ≈ 48px). The default size
// is the guild-icon-store resolution (up to 4096px), which would
// ship a much larger asset than gets rendered. Per-DM bandwidth
// savings × the dispatch loop = a real win at scale.
function resolveGuildProvenance(interaction) {
  return {
    guildName: interaction?.guild?.name,
    guildIconUrl: interaction?.guild?.iconURL({ size: 64 }) ?? undefined,
  };
}

// Resolve a recipient's per-guild display alias: guild nickname >
// globalName > username. Falls back through whatever we have on the
// recipient object so the helper works for User-from-UserSelect,
// GuildMember-from-channel-members, and the {id, username} shape
// produced by handleAddRecipients. Returns the PLAIN sanitized form
// (NFKC + bidi/zero-width strip + length cap, no markdown escape).
// Callers that render into Discord message content must wrap in
// escapeDiscordMarkdown; callers writing to a plain-text surface
// (e.g. revoked-users.txt) use the return value directly.
function resolveRecipientAlias(r, interaction) {
  const member = interaction?.guild?.members?.cache?.get(r.id);
  const raw = member?.displayName
    ?? r?.displayName
    ?? r?.user?.displayName
    ?? r?.username
    ?? `user-${r?.id}`;
  return sanitizeDisplayNamePlain(raw);
}

// Resolve role IDs to display names for the `roleMentionsDeniedNames`
// warning surface (#326). `||` (not `??`) so empty-string names —
// forged interaction / future API shape — also fall through to
// `unknown-role` rather than rendering `@` with broken backticks.
function resolveRoleNames(guild, ids) {
  if (!ids?.length) return [];
  return ids.map((id) => guild?.roles?.cache?.get(id)?.name || 'unknown-role');
}

// --- Shared DM delivery payload builder ---
// Builds the {embeds, components} payload for a per-recipient DM. The
// embed copy is intentionally evocative ("opened a door", "Closes")
// rather than literal ("shared a file with you") — the brand goal is to
// convey the qURL hidden-layer model, not just announce a file transfer.
// The qURL link is rendered as a `🚪 Step Through` Link button rather
// than a bare URL field; recipients click the button to open the link
// in their default browser.
//
// `senderAlias` is the sender's friendly display name (Discord nickname
// > globalName > username) sourced from resolveSenderAlias.
// `personalMessage` is optional caller-provided context; if present, it
// renders as an italicized blockquote between the sender line and the
// expiry line.
//
// Returns the full Discord message options object (`embeds` + `components`)
// rather than just the embed, since the button is not part of the embed
// — it lives in a top-level component row alongside it. Callers pass the
// returned payload directly to `sendDM`.
//
// Rendered output (blank rows = Discord's natural section spacing,
// NOT literal `\n` separators — descLines.join('\n') is single-newline):
//
//     ┌─────────────────────────────────────────────────────────────┐
//     │  qURL · APP · Today at 2:47 PM                              │  (Discord-rendered header)
//     │                                                             │
//     │  Vik opened a door for you.                                 │  (description, line 1)
//     │  > "Quarterly numbers — for your eyes only."                │  (description, line 2 — optional italic blockquote)
//     │  🕐 Closes in 1 day                                         │  (description, line 3 — Discord <t:N:R>, auto-updates
//     │                                                             │   client-side to "in 16 hours" / "in 1 hour" / "1 hour ago")
//     │                                                             │
//     │  ┌──────────────────────────┐                               │
//     │  │   🚪 Step Through        │  (Link button — opens qURL)
//     │  └──────────────────────────┘                               │
//     └─────────────────────────────────────────────────────────────┘
//
// Discord's Link-style buttons are always grey/blurple; the green color
// in the design mockup would require a Success-style button + custom_id
// + interaction handler that redirects, which adds a click round-trip
// for marginal aesthetic gain. Sticking with Link button for this pivot.
// Single source of truth for the SlashCommandBuilder `addChoices(...)` —
// `EXPIRY_CHOICES` is derived from this map so dropdown labels and values
// cannot drift. The DM embed renders expiry as a Discord native relative
// timestamp (`<t:N:R>`, see buildDeliveryPayload) so the human label is
// only used in the slash-command picker, not at render time.
const EXPIRY_LABELS = {
  '30m': '30 minutes',
  '1h': '1 hour',
  '6h': '6 hours',
  '24h': '24 hours',
  '7d': '7 days',
};

const EXPIRY_CHOICES = Object.entries(EXPIRY_LABELS).map(([value, name]) => ({ name, value }));

// Pure predicate for the EXPIRY_LABELS closed-set membership check.
// `Object.prototype.hasOwnProperty.call` (NOT `EXPIRY_LABELS[v]`) is
// load-bearing: a caller-supplied `'toString'`/`'constructor'` key
// would otherwise pass a truthy check via prototype access.
// `git grep isValidExpiry` for call sites.
function isValidExpiry(v) {
  return Object.prototype.hasOwnProperty.call(EXPIRY_LABELS, v);
}

// Per-pick cap on UserSelectMenuBuilder.setMaxValues, set to Discord's
// max_values hard limit. Used by both the initial confirm-card picker
// and the post-send "Add Recipients" flow — keep them in lockstep.
const USER_SELECT_PER_PICK_CAP = 25;

// Shared render caps for warnings-block bullets that surface user-
// or admin-controlled strings (invalidTokens, roleMentionsDeniedNames).
// 10-bullet cap bounds the embed footprint under a forged-interaction
// enumeration attempt; 80-codepoint per-string cap keeps a single
// pathological role name / mention token from dwarfing the rest of
// the card. Hoisted here (not inline in renderRecipientWarnings) so
// the shared intent is explicit — one place to bump if Discord's
// embed budget changes.
const WARNING_LIST_DISPLAY_MAX = 10;
const WARNING_NAME_CODEPOINT_CAP = 80;

// Discord's protocol-level cap on select-menu max_values. Authoritative
// — both picker sites must include this in their Math.min so we never
// construct a menu Discord rejects at validation regardless of how
// USER_SELECT_PER_PICK_CAP or QURL_SEND_MAX_RECIPIENTS are configured.
const DISCORD_SELECT_MAX_VALUES_HARD_CAP = 25;

// Shared Google Maps URL patterns. `/qurl map`'s slash-option
// `location:` consumes these — extracted to a single source so a
// future "new URL shape" tweak only happens here.
//
// Each pattern bounds character-class repetition (`{1,500}`, `{1,32}`,
// etc.) to keep ReDoS-resistant against pathological inputs.
const MAPS_URL_PATTERNS = [
  /https?:\/\/(?:www\.)?google\.com\/maps\/(?:place|search|dir|@)[\w/.,@?=&+%-]{1,500}/,
  /https?:\/\/(?:goo\.gl\/maps|maps\.app\.goo\.gl)\/[\w-]{1,100}/,
  // Defense-in-depth: exclude `<>"'|\` from the embed-tail character
  // class. `isGoogleMapsURL` re-validates host and downstream sanitizers
  // escape rendering, so these chars aren't exploitable today — but
  // narrowing the regex here makes the bound tighter against URL
  // shapes that would never come from a legitimate maps embed link.
  /https?:\/\/(?:www\.)?google\.com\/maps\/embed\/v1\/\w{1,32}\?[^\s<>"'|\\`]{1,500}/,
];

// decodeURIComponent throws URIError on malformed %-encoding (e.g. %ZZ).
// Swallow + return the raw string — a garbled label is preferable to a
// crashed command handler.
function safeDecodeURIComponent(s) {
  try { return decodeURIComponent(s); } catch { return s; }
}

// Parse a free-form `location:` input into one of three shapes:
//   - URL input → { locationUrl, locationName }
//   - place_id sentinel from autocomplete → { placeId }
//   - free text → { text }
//
// Callers pass the result to resolveLocation, which hits Places for the
// sentinel + text branches. The URL branch synchronously short-circuits.
function parseLocationInput(rawInput) {
  const decodedPlaceId = decodePlaceIdSentinel(rawInput);
  if (decodedPlaceId !== null) {
    return { locationUrl: null, locationName: null, placeId: decodedPlaceId };
  }
  let detectedUrl = null;
  for (const pattern of MAPS_URL_PATTERNS) {
    const match = rawInput.match(pattern);
    if (match) { detectedUrl = match[0]; break; }
  }
  if (detectedUrl && isGoogleMapsURL(detectedUrl)) {
    const queryMatch = detectedUrl.match(/[?&]q=([^&]+)/);
    // `?api=1&query=…` is the canonical "Maps URL" form (the one
    // resolveLocation constructs). Match it too so a sender who
    // re-shares a previously-constructed qURL map URL still gets a
    // name extracted instead of falling back to "location" in the
    // recipient embed.
    const apiQueryMatch = detectedUrl.match(/[?&]query=([^&]+)/);
    const placeMatch = detectedUrl.match(/\/place\/([^/@]+)/);
    let name = null;
    if (queryMatch) name = safeDecodeURIComponent(queryMatch[1].replace(/\+/g, ' '));
    else if (apiQueryMatch) name = safeDecodeURIComponent(apiQueryMatch[1].replace(/\+/g, ' '));
    else if (placeMatch) name = safeDecodeURIComponent(placeMatch[1].replace(/\+/g, ' '));
    return { locationUrl: detectedUrl, locationName: name };
  }
  return {
    locationUrl: null,
    locationName: null,
    text: rawInput,
  };
}

// Failure modes of resolveLocation. handleQurlMap maps each to a
// distinct user-facing reply.
const RESOLVE_REASON = Object.freeze({
  NO_API_KEY: 'no_api_key',
  NOT_FOUND: 'not_found',
  ERROR: 'error',
});

// Discriminated result of resolveLocation. Callers check `.ok`:
//   { ok: true,  locationUrl, locationName }            (success)
//   { ok: false, reason: RESOLVE_REASON.<...> }         (failure)
async function resolveLocation(parsed) {
  if (parsed.locationUrl) {
    return { ok: true, locationUrl: parsed.locationUrl, locationName: parsed.locationName };
  }
  if (!config.GOOGLE_MAPS_API_KEY) {
    return { ok: false, reason: RESOLVE_REASON.NO_API_KEY };
  }
  try {
    const place = parsed.placeId
      ? await getPlaceDetails(parsed.placeId)
      : await findPlaceFromText(parsed.text);
    if (!place || !place.placeId) {
      return { ok: false, reason: RESOLVE_REASON.NOT_FOUND };
    }
    return {
      ok: true,
      locationUrl: buildPlaceUrl(place.name, place.placeId),
      locationName: place.name || place.address || null,
    };
  } catch (err) {
    logger.warn('resolveLocation: Places API call failed', {
      kind: parsed.placeId ? 'placeId' : 'text',
      error: err && err.message,
    });
    return { ok: false, reason: RESOLVE_REASON.ERROR };
  }
}

// Trust-signal Link button surfaced beside Step Through on every
// delivery DM. Mirrors a browser address bar's "click the lock to
// verify" affordance: a first-time recipient can hit qURL's public
// landing page to confirm the brand exists before clicking Step
// Through.
function buildTrustButton() {
  return new ButtonBuilder()
    .setStyle(ButtonStyle.Link)
    // U+1F6E1 + U+FE0F. The FE0F variation selector forces emoji-
    // presentation rendering — without it some clients (older Linux,
    // some web fallbacks) render the bare shield codepoint as a
    // text-style glyph rather than the colored emoji.
    .setEmoji('🛡️')
    .setLabel('What is qURL?')
    .setURL(TRUST.LANDING_URL);
}

// The Step Through Link button is the primary action on every DM.
// Extracted as a factory so the bulk-path call site can compose
// per-link buttons without round-tripping through buildDeliveryPayload
// (which always pairs it with a trust button — wasteful when the
// bulk path packs a single trust button at the bottom for the whole
// message). 🚪 emoji ties the "opened a door for you" copy to the
// action — author row, embed accent, and button emoji all reinforce
// one metaphor.
function buildStepThroughButton(qurlLink) {
  return new ButtonBuilder()
    .setStyle(ButtonStyle.Link)
    .setEmoji('🚪')
    .setLabel('Step Through')
    .setURL(qurlLink);
}

// Pack a list of qURL links into the bulk-path component rows:
// one Step Through button per link plus one trust button at the
// end, chunked 5-per-ActionRow (Discord's per-row cap). The
// dispatch caller upstream caps links at 10 (Discord's 10-embed-
// per-message limit), so the worst case is 11 buttons → 3 rows.
// Behavior at N=1 produces the same single-row [Step Through,
// What is qURL?] shape executeSendPipeline emits, so a /qurl send
// followed by /qurl add-recipients renders identically on the
// recipient side.
//
// PRECONDITION: 1 <= qurlLinks.length <= 10.
//   - Empty: a single row containing only the trust button would
//     ship without any embeds, which Discord rejects.
//   - >10: N + 1 buttons across 5-per-row chunks exceeds Discord's
//     5-ActionRow message cap (e.g. N=11 → 3 rows, N=24 → 5 rows,
//     N=25 → 6 rows = API rejection). The upstream caller in
//     handleAddRecipients already caps via `links.slice(0, 10)` to
//     match Discord's 10-embed-per-message limit; the throw below
//     pins both ends of the contract for any future caller that
//     misses the docstring.
function packBulkDeliveryComponents(qurlLinks) {
  if (!Array.isArray(qurlLinks) || qurlLinks.length === 0) {
    throw new Error('packBulkDeliveryComponents: qurlLinks must be a non-empty array');
  }
  if (qurlLinks.length > 10) {
    throw new Error(`packBulkDeliveryComponents: qurlLinks length ${qurlLinks.length} exceeds the 10-link cap`);
  }
  const allButtons = [
    ...qurlLinks.map(buildStepThroughButton),
    buildTrustButton(),
  ];
  const rows = [];
  for (let i = 0; i < allButtons.length; i += 5) {
    rows.push(new ActionRowBuilder().addComponents(allButtons.slice(i, i + 5)));
  }
  return rows;
}

// Cap a string at `maxUtf16Units` UTF-16 code units (matching how
// Discord's API measures `embed.author.name` and similar limits),
// trimming a trailing lone high surrogate if the cap lands mid-pair
// so the result is never an invalid UTF-16 sequence. Discord renders
// a lone surrogate as tofu; the trim ensures a clean cut.
function capUtf16Units(s, maxUtf16Units) {
  if (s.length <= maxUtf16Units) return s;
  let truncated = s.slice(0, maxUtf16Units);
  const lastCharCode = truncated.charCodeAt(truncated.length - 1);
  // High-surrogate range (U+D800..U+DBFF): if the cap split a pair,
  // drop the orphan so the result decodes cleanly. The low-surrogate
  // case (U+DC00..U+DFFF at the boundary) is unreachable by
  // construction — `slice(0, n)` would have excluded a low surrogate
  // at position n, so the high half is the only orphan possible at
  // position n-1. The asymmetric check is correct, not a bug.
  if (lastCharCode >= 0xD800 && lastCharCode <= 0xDBFF) {
    truncated = truncated.slice(0, -1);
  }
  return truncated;
}

// Composes the embed only (no button row). Split out so the bulk-path
// dispatch in handleAddRecipients can build N embeds + N step-through
// buttons + 1 trust button without round-tripping through
// buildDeliveryPayload (which always allocates a trust button per
// call). The single-path call sites use buildDeliveryPayload below.
function buildDeliveryEmbed({ senderAlias, guildName, guildIconUrl, expiresAt, personalMessage }) {
  // Discord's `<t:N:R>` markdown wants a positive integer Unix-seconds
  // value; anything else renders a misleading recipient surface (e.g.
  // `<t:0:R>` → "56 years ago", `<t:undefined:R>` → literal text,
  // `<t:1735689600.5:R>` → parse failure). Match the contract exactly
  // and fail loud — call sites all compute via `Math.floor(...)` so no
  // legitimate input is rejected. `typeof` in the throw closes the
  // null-vs-undefined-vs-object triage gap for operators.
  if (!Number.isInteger(expiresAt) || expiresAt <= 0) {
    const got = String(expiresAt);
    const gotType = typeof expiresAt;
    throw new Error(
      `buildDeliveryEmbed: expiresAt must be a positive integer `
      + `Unix-seconds number (got ${got}, typeof=${gotType})`
    );
  }

  // Author row is plaintext (no markdown rendering), so the PLAIN
  // sanitization variant is the right tool for both sender and
  // guildName halves — backslash-escapes would render literally
  // here, but the bidi/zero-width spoof defense still applies.
  // guildName opts into an empty fallback ({ fallback: '' }) so an
  // all-strip hostile name (RLO/ZWSP/BOM only) collapses to "" and
  // the author row falls back to sender-only — rather than rendering
  // the nonsense `Vik · Someone` that the DISPLAY_NAME_FALLBACK
  // default would produce, degrading the trust signal on the exact
  // hostile input the strip exists to defend.
  //
  // Combined-length budget: 64 (sender cap) + 3 (` · `) + 64 (guild
  // cap) = 131 codepoints. Discord's author.name limit is 256, but
  // measured in UTF-16 code units (not codepoints), so a worst-case
  // mix of 64 surrogate-pair emoji on each half is 64×2 + 3 + 64×2
  // = 259 UTF-16 units — over the cap. Vanishingly rare in practice
  // (would need both halves attacker-controlled and all-emoji), but
  // the slice-and-trim below caps the final string at 256 UTF-16
  // units codepoint-aware so the API never rejects the embed.
  const safeSenderPlain = sanitizeDisplayNamePlain(senderAlias);
  const safeGuildName = sanitizeDisplayNamePlain(guildName, { fallback: '' });
  const authorName = capUtf16Units(
    safeGuildName ? `${safeSenderPlain} · ${safeGuildName}` : safeSenderPlain,
    256,
  );

  // Discord renders fields BELOW the description, so all lines (optional
  // personal message + expiry) must live in one setDescription block —
  // an addFields personal-message would land after the expiry line, not
  // between description and expiry. Folding also strips addFields'
  // vertical padding, keeping the Step Through button close.
  //
  // `<t:N:R>` is Discord's client-side relative-time markdown. Pick
  // the verb at render time so already-expired rows read naturally
  // ("Closed 3 days ago") while future rows keep the live countdown
  // copy ("Closes in 1 day").
  //
  // CONTRACT: `personalMessage` arrives pre-sanitized. `/qurl send`
  // and `/qurl map` pipe raw input through `sanitizeMessage`
  // (markdown escape + @-mention strip) before constructing this
  // payload, and the addRecipients path reads from
  // `sendConfig.personal_message` which was sanitized at write time.
  // Raw interpolation below is safe ONLY because of that upstream
  // pass. A future caller that bypasses sanitizeMessage (or a DB row
  // read that skips re-sanitize) would silently regress to markdown
  // injection — keep the contract.
  //
  // Discord blockquote (`> `) only quotes one line and italic (`*…*`)
  // does not span newlines, so a multi-line message would render with
  // only the first line styled. Flatten newlines to a space so the
  // recipient sees one tidy quote — matches the design mockup which
  // shows the message as a single-line styled box. 280-codepoint cap
  // keeps the embed visually compact. Headroom: 280 codepoints ≤ 1120
  // UTF-8 bytes + ~10 chars of `> *"…"*` wrapper + ~30-char expiry
  // line ≪ Discord's 4096-char description cap.
  const descLines = ['opened a door for you.'];
  if (personalMessage) {
    // Skip the styled-blockquote line if the input collapses to an
    // empty string after newline-flatten + trim (e.g. "  \n \n  ").
    // The call sites already pass `sanitizeMessage(...) || null` so
    // an empty input arrives as null and short-circuits the outer
    // `if (personalMessage)`. This guard only matters if a future
    // caller bypasses that contract — belt-and-braces vs rendering
    // a visible-but-empty `> *""*` row between sender and expiry.
    //
    // Order is intentional: cap → replace → trim. The 280-codepoint
    // cap applies to RAW input (bounds the work done by replace/trim
    // against a 10KB single-line pathological input) rather than to
    // the rendered output. A future "fix" to `replace().trim()` then
    // cap would be a subtly different contract — the visible output
    // would be capped at 280 but unbounded work would happen before.
    //
    // `Array.from(s).slice(0, 280).join('')` is codepoint-aware: a
    // 4-byte emoji (surrogate pair) is one element in the resulting
    // array, so the cap can't split it into a lone high surrogate.
    // 280 codepoints = at most 560 UTF-16 units = still well under
    // Discord's 4096-char description cap. Mirrors the cap pattern
    // sanitizeDisplayName uses for senderAlias.
    const capped = Array.from(personalMessage).slice(0, 280).join('').replace(/[\r\n]+/g, ' ').trim();
    if (capped) descLines.push(`> *"${capped}"*`);
  }
  const expiryVerb = expiresAt <= Math.floor(Date.now() / 1000) ? 'Closed' : 'Closes';
  descLines.push(`🕐 ${expiryVerb} <t:${expiresAt}:R>`);

  // Author row is the embed's "address bar" — anchored top, visually
  // distinct from the description, the closest analog Discord offers
  // to a browser's origin display. Icon is only attached when the
  // guild has one — bare `undefined` (NOT null) is what discord.js
  // wants for "no icon"; some versions stringify null into the URL slot.
  const authorOpts = { name: authorName };
  if (guildIconUrl) authorOpts.iconURL = guildIconUrl;

  return new EmbedBuilder()
    .setColor(COLORS.QURL_BRAND)
    .setAuthor(authorOpts)
    .setDescription(descLines.join('\n'))
    // Footer reinforces the destination domain — same trust idea as a
    // browser showing where a link points before you click. Literal
    // string (not interpolated from qurlLink) keeps the recipient
    // surface stable even if a future minted link uses a subdomain.
    .setFooter({ text: `opens ${TRUST.DESTINATION_DOMAIN}` });
}

// Convenience wrapper composing the embed + one ActionRow holding
// [Step Through, What is qURL?]. Used by the single-path call site
// (executeSendPipeline); the bulk path composes from the primitives
// directly so it can pack N step-throughs with one shared trust button.
function buildDeliveryPayload({ senderAlias, guildName, guildIconUrl, qurlLink, expiresAt, personalMessage }) {
  const embed = buildDeliveryEmbed({ senderAlias, guildName, guildIconUrl, expiresAt, personalMessage });
  const components = [new ActionRowBuilder().addComponents(buildStepThroughButton(qurlLink), buildTrustButton())];
  return { embeds: [embed], components };
}

// CONTRACT: `components: []` MUST be passed explicitly. Discord's
// PATCH /messages does NOT clear fields that aren't supplied —
// omitting components would leave the original Step Through button
// live in the recipient's DM, pointing at a now-dead qURL resource.
function buildRevokedDMPayload({ senderAlias }) {
  const safeSender = sanitizeDisplayName(senderAlias);
  const embed = new EmbedBuilder()
    .setColor(COLORS.QURL_BRAND)
    .setDescription(`🚪 **${safeSender}** closed the door.\nThis qURL is no longer active.`);
  return { embeds: [embed], components: [] };
}

// Records the outcome of a sendDM dispatch attempt into qurl_sends.
// Happy path coalesces dm_status='sent' + DM refs into one DDB Update
// so the hot dispatch path stays at one write per recipient. Failure
// path is status-only — there's no message to edit later.
//
// NEVER throws on a delivered DM. If the DDB write fails (outage,
// throttle, validation), DISPATCH_PERSIST_FAILED fires + an error is
// logged, but the function returns normally so the caller continues
// to report the recipient as delivered. Throwing here would convert
// a real DM into a "could not be reached" line on the operator's
// reply — worse than the bookkeeping miss.
//
// CALLER INVARIANT: every call site MUST await db.recordQURLSendBatch
// before invoking the dispatch loop that calls this function. The
// underlying UpdateCommand has no attribute_exists guard (#366), so
// without that ordering a stale recipient_discord_id would create an
// orphan row. executeSendPipeline + handleAddRecipients both honor
// this — recordQURLSendBatch awaits to completion (with an early-
// return on throw) before the batchSettled dispatch loop starts.
async function persistDispatchResult(sendId, recipientDiscordId, result) {
  // Wraps the DDB call so a write failure can't propagate up as a
  // failed dispatch. `delivered` ⇒ the DM is real and the operator
  // should still see this recipient as reached.
  const persist = async (op, delivered) => {
    try {
      await op();
    } catch (err) {
      if (delivered) {
        logger.audit(AUDIT_EVENTS.DISPATCH_PERSIST_FAILED, { send_id: sendId });
      }
      logger.error('persistDispatchResult: qurl_sends write failed', {
        sendId, recipientDiscordId, delivered, errorMessage: err.message,
      });
    }
  };

  if (result.ok === true && result.channelId && result.messageId) {
    await persist(
      () => db.markSendDMDelivered(sendId, recipientDiscordId, result.channelId, result.messageId),
      true,
    );
    return;
  }
  if (result.ok === true) {
    // sendDM said ok but discord.js omitted channelId / messageId.
    // Record SENT so DDB delivery rollups stay faithful; the revoke
    // path's missing-refs guard skips the DM edit naturally.
    // dispatch_sent_no_refs is a CANARY (not a DISPATCH_SENT
    // subtractor) — emitted before the persist so a DDB outage
    // can't mask a discord.js shape-drift.
    logger.audit(AUDIT_EVENTS.DISPATCH_SENT_NO_REFS, { send_id: sendId });
    logger.warn('sendDM resolved ok but missing channelId/messageId — recording as sent (revoke edit will skip)', {
      sendId, recipientDiscordId,
      hasChannelId: Boolean(result.channelId),
      hasMessageId: Boolean(result.messageId),
    });
    await persist(
      () => db.updateSendDMStatus(sendId, recipientDiscordId, DM_STATUS.SENT),
      true,
    );
    return;
  }
  // sendDM failed — the DM is not real. A bookkeeping error here is
  // strictly a dropped status update; the recipient was never reached.
  await persist(
    () => db.updateSendDMStatus(sendId, recipientDiscordId, DM_STATUS.FAILED),
    false,
  );
}

// --- Link status monitor ---
// Track live monitors so a burst of `/qurl send` + `/qurl map` commands
// can't stack more than MAX_CONCURRENT_MONITORS poll loops. When we
// cross the cap, the oldest monitor is stopped to make room (the user
// can still `/qurl revoke`; they just stop seeing live status updates in
// the original message).
const activeMonitors = new Set();

// Discord ephemeral messages can ONLY be edited via the interaction
// webhook token (which expires 15 min after the interaction is created),
// not via bot-token channel PATCH. Both /qurl send and /qurl map
// deferReply ephemeral, so the confirmation is unreachable once the
// token expires. There's no cross-token fallback — we just cap the
// monitor below the 15-min cliff so we don't waste poll loops
// against a dead token.

function monitorLinkStatus(sendId, interactionArg, qurlLinksArg, recipientsArg, expiresIn, baseMsg, buttonRowArg, delivered) {
  // Rebind params as closure-mutable so stop() can null them out for GC.
  // Long-running monitors (up to MAX_MONITOR_DURATION_MS ×
  // MAX_CONCURRENT_MONITORS=50) otherwise pin interaction/recipients/
  // buttonRow in the poll-loop closure.
  let interaction = interactionArg;
  let qurlLinks = qurlLinksArg;
  let recipients = recipientsArg;
  let buttonRow = buttonRowArg;
  let currentBaseMsg = baseMsg;
  let stopped = false;
  let timer = null;
  let expectedCount = delivered;

  // Seed tracked qurl_ids from the in-memory qurlLinks. Each linkStatus
  // entry pre-populates with the recipient's username so the first
  // webhook-fed tick can flip status → 'opened' without losing the
  // label. Empty qurlId on ANY link degrades the whole monitor — a
  // partial-attribution counter would mislead worse than no counter.
  const trackedQurlIds = new Set();
  const linkStatus = new Map();
  let degradedAtConstruction = false;
  for (let i = 0; i < qurlLinks.length; i++) {
    const qid = qurlLinks[i].qurlId;
    if (!qid) { degradedAtConstruction = true; continue; }
    trackedQurlIds.add(qid);
    const username = recipients[i] ? recipients[i].username : `user-${i + 1}`;
    linkStatus.set(qid, { status: 'pending', username });
  }
  // Re-derived each tick so addRecipients() with a missing qurlId can
  // re-degrade the monitor mid-life. Set during addRecipients too.
  let viewCounterDegraded = degradedAtConstruction;
  if (viewCounterDegraded) {
    logger.warn('Monitor view counter degraded — some links missing qurl_id', {
      sendId, missing: qurlLinks.filter(l => !l.qurlId).length, total: qurlLinks.length,
    });
  }

  // Hoisted so the createHandleViewUpdate factory closes over the
  // live `let` binding rather than a pre-initialized TDZ slot — a
  // future synchronous-handler refactor would otherwise hit
  // ReferenceError on getViewed/setViewed.
  let viewed = 0;
  let allDone = false;

  // View-update push (feat #60). Shared callback per monitor —
  // registered against every tracked qurl_id; render goes through
  // linkStatus mutation + safeEdit (NOT runTick) to avoid the
  // per-event DDB BatchGet. Polling tick remains the correctness
  // primitive. Handler factory in view-update-handler.js so the
  // state matrix is unit-testable without a full monitor closure.
  //
  // Unregister at stop() iterates `trackedQurlIds` directly (set
  // cleared AFTER the unregister loop). Registry.unregister is a
  // no-op for keys never registered (e.g. when the flag is off), so
  // iterating the superset is safe.
  const handleViewUpdate = createHandleViewUpdate({
    sendId,
    linkStatus,
    getButtonRow: () => buttonRow,
    isStopped: () => stopped,
    isViewCounterDegraded: () => viewCounterDegraded,
    hasInteraction: () => !!interaction,
    getViewed: () => viewed,
    setViewed: (n) => { viewed = n; },
    getExpectedCount: () => expectedCount,
    buildStatusMsg,
    safeEdit,
    onAllDone: () => {
      allDone = true;
      clearTimeout(timer);
    },
    logger,
  });
  // Hoist the flag read once per monitor — registerViewUpdateFor
  // is called up to QURL_SEND_MAX_RECIPIENTS times (default 20000) on
  // construction + once per /qurl add. Re-reading config on every
  // call is negligible but the hoist reads cleaner.
  const viewUpdatePushEnabled = config.ENABLE_VIEW_UPDATE_PUSH;
  function registerViewUpdateFor(qurlId) {
    if (!viewUpdatePushEnabled) return;
    viewUpdateRegistry.register(qurlId, handleViewUpdate);
  }
  for (const qurlId of trackedQurlIds) {
    registerViewUpdateFor(qurlId);
  }

  // Bumped by addRecipients. A tick whose getQurlViews resolves after
  // generation advanced must skip its render — the views map was built
  // against the pre-add tracked set and could under-report.
  let trackingGeneration = 0;

  const control = {
    // newLinks: Array<{qurlId, username}> aligned per-recipient. Must
    // seed `linkStatus` AND `trackedQurlIds` together — extending only
    // the tracked set means runTick's view-flip lookup misses the new
    // qurl_ids and the counter never advances for /qurl add recipients.
    addRecipients(count, newLinks) {
      // Early-out if this monitor has already been stopped. Without
      // this guard a post-stop addRecipients call would extend
      // expectedCount, linkStatus, AND register new view-update
      // callbacks against the registry — the latter wouldn't be
      // unregistered (stop() has already iterated trackedQurlIds),
      // pinning the closure (linkStatus + safeEdit) until process
      // restart. Pre-#60 fields (expectedCount,
      // linkStatus) also leaked into a dead monitor; this guard
      // closes both the new + pre-existing leak surfaces.
      if (stopped) return;
      expectedCount += count;
      if (Array.isArray(newLinks)) {
        for (const item of newLinks) {
          const qid = item && item.qurlId;
          if (!qid) {
            // Mirror the construction-time warn — an operator chasing
            // "why is the counter blank?" needs a breadcrumb when the
            // degraded flip happens mid-life via /qurl add, not just
            // at the original send.
            if (!viewCounterDegraded) {
              logger.warn('Monitor view counter degraded mid-life — addRecipients link missing qurl_id', { sendId });
            }
            viewCounterDegraded = true;
            continue;
          }
          if (!trackedQurlIds.has(qid)) {
            trackedQurlIds.add(qid);
            linkStatus.set(qid, { status: 'pending', username: item.username || 'unknown' });
            // Register the new qurl_id for view-update push (feat #60).
            // Symmetric with the construction-time loop above — the
            // tracked set and the registry must extend together or the
            // new recipients never get sub-second updates (the polling
            // path catches them on the next interval regardless).
            registerViewUpdateFor(qid);
          }
        }
      }
      trackingGeneration++;
      // /qurl add is an explicit signal the send is NOT idle, so re-enter
      // the dense early phase. Refreshing earlyPhaseUntil (NOT startTime,
      // which governs the 14-min life cap) speeds the cadence back up
      // without extending the monitor's lifetime. nextPollDelay() reads
      // this at the NEXT reschedule.
      earlyPhaseUntil = Date.now() + EARLY_POLL_WINDOW_MS;
      // Two cases:
      //   - Monitor already settled (all initial recipients viewed → the
      //     loop stopped rescheduling on allDone): re-arm via arm() so the
      //     new recipients' views can flip — without this, /qurl add on a
      //     resolved send leaves the counter frozen for the monitor's life.
      //     First post-add tick fires at FIRST_POLL_DELAY_MS (3s).
      //   - Monitor still pending (loop still running): we deliberately do
      //     NOT clearTimeout+re-arm. The live tick already owns `timer`;
      //     restarting would either fire a redundant extra tick or (if a
      //     tick is mid-await) need the loopGen guard to avoid orphaning a
      //     timer. The running loop already picks up the refreshed
      //     earlyPhaseUntil at its next reschedule, so a /qurl add past the
      //     90s window waits at most one in-flight steady interval (≤60s)
      //     before the dense cadence resumes — an accepted, bounded
      //     asymmetry vs. paying the double-timer complexity for the rarer
      //     pending-add case.
      if (allDone && !stopped) {
        allDone = false;
        clearTimeout(timer);
        arm(FIRST_POLL_DELAY_MS);
      }
    },
    stop() {
      if (stopped) return;
      stopped = true;
      clearTimeout(timer);
      activeMonitors.delete(control);
      // Unregister the shared callback from every tracked qurl_id so
      // the registry doesn't pin this monitor's closure state past
      // stop() (feat #60). Load-bearing: a long-running monitor that
      // never unregisters would otherwise hold linkStatus + safeEdit
      // references via the shared closure until process restart.
      // Iterates trackedQurlIds directly (no parallel bookkeeping
      // array) — set cleared in the next two lines, so order matters.
      for (const qurlId of trackedQurlIds) {
        viewUpdateRegistry.unregister(qurlId, handleViewUpdate);
      }
      linkStatus.clear();
      trackedQurlIds.clear();
      interaction = null;
      qurlLinks = null;
      recipients = null;
      buttonRow = null;
    },
    updateBaseMsg(msg) { currentBaseMsg = msg; },
    getFullMsg() { return buildStatusMsg(); },
  };

  const expiryMs = expiryToMs(expiresIn);
  // 14-min cap matches the practical edit window. The interaction
  // webhook token expires at ~15 min from interaction creation; both
  // /qurl send + /qurl map deferReply ephemeral, and ephemeral
  // messages can't be edited via any other token. Past the cap the
  // poll loop would only burn cycles against a dead token. Store-
  // side view recording (recordQurlView) is unaffected — webhooks
  // keep landing for the link's full lifetime; the sender's confirm-
  // message counter just freezes after the cap. Links themselves
  // still expire at the API level regardless of the monitor cap.
  const MAX_MONITOR_DURATION_MS = 14 * 60 * 1000;
  const maxMonitorMs = Math.min(expiryMs + 60000, MAX_MONITOR_DURATION_MS);

  // Steady-state cadence once a send has been live past the early
  // window — bounds the sustained cost of an idle monitor (one DDB
  // BatchGet per tick). Floored at 15s, capped at 60s, scaled to 1/10
  // of the link's expiry so short-lived links poll more often.
  const steadyPollInterval = Math.max(15000, Math.min(60000, expiryMs / 10));
  // Fast first tick catches sub-second self-destruct sends where the
  // recipient burns the link before any interval would fire.
  const FIRST_POLL_DELAY_MS = 3000;
  // Dense early-poll phase. The webhook fast-path
  // (routes/qurl-webhook.js) owns normal sub-second latency, but the
  // cross-replica PATCH primitive is live-only. Keep #839's 5s dense
  // backstop until that primitive is verified so a bad fast-path
  // assumption cannot regress send counters below the pre-PR fallback.
  // The poll also self-heals failed edits and restores expand/collapse
  // content-only flicker.
  //
  // Cost: this replaces the old pollCount decay (which throttled an idle
  // monitor toward every-4th-tick), so the steady phase is a flat
  // steadyPollInterval (≤60s). Discord-edit volume does NOT scale with
  // poll rate: runTick edits ONLY on a real pending→opened transition
  // (`changed`) and each link flips once, so total edits over the
  // monitor's life are bounded by the count of distinct recipient views
  // (≤ QURL_SEND_MAX_RECIPIENTS, default 20000 — NOT 50), spread across
  // the link's whole lifetime. At ≥5s spacing in the early window this is
  // still far under Discord's editReply rate limit. (The webhook
  // fast-path, which fires per-view rather than per-tick, is the path
  // that needs the burst coalescing above; this poll is naturally rate-
  // limited by its own poll tick spacing.)
  const EARLY_POLL_INTERVAL_MS = 5000;
  const EARLY_POLL_WINDOW_MS = 90000;
  const startTime = Date.now();
  // Anchor for the dense early phase. Distinct from startTime (which
  // governs the 14-min life cap via maxMonitorMs) so addRecipients can
  // refresh the dense phase for newly-added recipients WITHOUT extending
  // the monitor's life past the interaction-token TTL.
  let earlyPhaseUntil = startTime + EARLY_POLL_WINDOW_MS;
  // Delay until the next tick: dense while inside the early window,
  // steady after. Read at each reschedule so a mid-life addRecipients
  // re-entry into the dense phase takes effect immediately.
  const nextPollDelay = () => (Date.now() < earlyPhaseUntil ? EARLY_POLL_INTERVAL_MS : steadyPollInterval);
  const isTerminated = () => stopped || allDone || Date.now() - startTime > maxMonitorMs;

  // Best-effort edit through the interaction webhook token. After the
  // 14-min monitor cap, this would 401 against an expired token —
  // logIgnoredDiscordErr swallows it. Recipients see status from
  // their DM, so the sender's frozen counter past cap is just a
  // dashboard nicety, not a load-bearing surface.
  // Returns true ONLY when the edit confirmed. The runTick counter path
  // gates its monotonic-floor advance (tryAdvanceRenderedCount) on this —
  // commit-after-edit, same invariant the webhook fast-path holds — so a
  // failed render must NOT advance the floor (else the fast-path's N<=L
  // skip would strand a count that was never displayed: stuck-counter).
  // The terminal/expand callers ignore the return, so this stays
  // non-breaking for them.
  async function safeEdit(payload) {
    if (!interaction) return false;
    return interaction.editReply(payload).then(() => true).catch((err) => {
      logIgnoredDiscordErr(err);
      return false;
    });
  }

  // `viewed` already declared above the createHandleViewUpdate factory
  // call so the factory closes over the live binding. Closure-tracked
  // so we don't re-walk linkStatus on each render; incremented when a
  // link flips pending → opened in either runTick OR the view-update
  // handler; never decreases.

  // No explicit `expired` state in the rendered counter: qurl.accessed
  // is the only event we receive, and the 1h monitor cap covers the
  // practical "user never opened it" window. Pre-PR-455 modelled
  // expired explicitly via upstream polling; the webhook-only world
  // can't observe expiration without a separate qurl.expired
  // subscription (out of scope).
  // Delegates to the pure renderViewCounter so the monitor's render and
  // PR-B's off-monitor fast-path render stay byte-identical. The closure
  // still owns the LIVE inputs (currentBaseMsg/viewed/expectedCount
  // mutate across ticks + addRecipients; viewCounterDegraded can flip
  // mid-life) — buildStatusMsg snapshots them at call time and hands them
  // to the pure fn.
  function buildStatusMsg(viewedOverride = viewed) {
    return renderViewCounter({
      baseMsg: currentBaseMsg,
      viewed: viewedOverride,
      expectedCount,
      degraded: viewCounterDegraded,
    });
  }

  // Non-overlapping ticks: arm()/tick() below reschedule the NEXT tick
  // only after the current runTick (including its single `await
  // db.getQurlViews`) resolves, so two ticks can never be in flight at
  // once — the self-destruct-era setInterval concurrency race is gone.
  // The synchronous `status === 'opened'` guard at the flip site stays
  // as defense-in-depth (and to no-op the view-update push path racing
  // a poll), so `viewed++` still can't double-count.
  const runTick = async () => {
    if (isTerminated()) {
      clearTimeout(timer);
      if (!interaction) return;
      const finalMsg = buildStatusMsg() + '\n(Use `/qurl revoke` to revoke later)';
      await safeEdit({ content: finalMsg, components: [] });
      return;
    }
    if (viewCounterDegraded || trackedQurlIds.size === 0) return;
    try {
      const genAtStart = trackingGeneration;
      const views = await db.getQurlViews([...trackedQurlIds]);
      // addRecipients during the await invalidates this snapshot.
      if (trackingGeneration !== genAtStart) return;
      if (!interaction) return;

      // Iterate the (sparse) views map instead of the full linkStatus
      // set — usually only a handful of qurls have actually been
      // accessed, vs. up to 50 tracked links.
      let changed = false;
      for (const [qurlId, v] of views) {
        if (!(v.accessCount > 0)) continue;
        const current = linkStatus.get(qurlId);
        if (!current || current.status === 'opened') continue;
        linkStatus.set(qurlId, { ...current, status: 'opened' });
        viewed++;
        changed = true;
      }
      if (changed) {
        // Clamp poll renders to the persisted display floor. The fast-path
        // can strongly read/render N before this replica's eventual
        // getQurlViews catches up; rendering below last_rendered_count would
        // create a visible N -> lower -> N flicker.
        let rendered = viewed;
        try {
          rendered = Math.max(rendered, await db.getSendRenderedCount(sendId));
        } catch (floorErr) {
          logger.debug('Monitor floor-read failed; rendering local poll count', { sendId, error: floorErr.message });
        }
        const pending = Math.max(0, expectedCount - rendered);
        const ok = await safeEdit({ content: buildStatusMsg(rendered), components: pending > 0 ? [buttonRow] : [] });
        if (pending === 0) { allDone = true; clearTimeout(timer); }
        // Share the monotonic floor with the webhook fast-path. The poll
        // is now a first-class renderer of the settled count (coalescing
        // makes the burst tail poll-only), so advance last_rendered_count
        // to what we just DISPLAYED — only on a confirmed edit (`ok`),
        // mirroring the fast-path's commit-after-edit. Without this the
        // fast-path could read a stale lower N after the poll showed a
        // higher count and step the display BACKWARDS until the next tick.
        // CCFE is the normal "a concurrent fast-path already advanced
        // higher" case (returns false, ignored). Best-effort + wrapped so
        // a non-CCFE DDB throw can't surface as a misleading "poll failed"
        // or skip the allDone bookkeeping above. (tryAdvanceRenderedCount
        // also stamps last_rendered_at, arming the coalesce clock — so a
        // genuine NEW view landing within QURL_VIEW_COUNTER_COALESCE_MS
        // after this tick is coalesced by the fast-path's step 4b and
        // rendered by the webhook trailing flush, not stranded for the
        // next poll.) NOT advanced on the degraded
        // early-return or the terminal-freeze render — neither displays a
        // live counter, so advancing would strand it.
        if (ok) {
          try {
            await db.tryAdvanceRenderedCount(sendId, rendered);
          } catch (advErr) {
            logger.debug('Monitor floor-advance failed (non-CCFE); fast-path/poll self-heal on next tick', { sendId, error: advErr.message });
          }
        }
      }
    } catch (err) {
      logger.error('Link monitor poll failed', { sendId, error: err.message });
    }
  };
  // Self-rescheduling poll loop. A single `setTimeout` handle (never a
  // setInterval) reschedules the NEXT tick only after the current one
  // resolves — that's what makes ticks non-overlapping (see runTick's
  // comment) and lets the cadence shift between the dense early phase
  // and the steady phase at each reschedule via nextPollDelay(). One
  // handle means control.stop()'s clearTimeout(timer) always cancels
  // the live timer; the post-tick isTerminated() guard skips
  // rescheduling when a tick already terminated the monitor (e.g. all
  // links resolved instantly). `arm()` is the single .unref() site,
  // shared by construction, the first tick's reschedule, and the
  // addRecipients re-arm.
  //
  // loopGen makes the single-handle invariant hold across an await.
  // arm() bumps it and the scheduled tick captures it; a tick that
  // resumes from `await runTick()` only reschedules if its capture still
  // matches. Without this, an addRecipients re-arm (which calls arm() →
  // a fresh timer) landing while a tick is suspended in its await would
  // leave the resumed stale tick to arm() a SECOND chain — two
  // concurrent poll loops (≈2× the BatchGet rate) until stop()/the
  // 14-min cap. It self-heals (stop() nulls interaction + linkStatus, so
  // the orphan's next runTick short-circuits and doesn't re-arm), so
  // there's no leak past stop — the guard just keeps the single-handle
  // invariant exact instead of relying on that self-heal.
  let loopGen = 0;
  function arm(delayMs) {
    const gen = ++loopGen;
    timer = setTimeout(() => tick(gen), delayMs);
    if (timer && timer.unref) timer.unref();
  }
  async function tick(gen) {
    await runTick();
    if (isTerminated() || gen !== loopGen) return;
    arm(nextPollDelay());
  }
  arm(FIRST_POLL_DELAY_MS);

  // Register this monitor in the global set. If we're over the cap, stop
  // the oldest-inserted monitor first (Set iteration order = insertion
  // order) so the new one can take its slot. The victim user can still
  // revoke links manually; they just lose live updates on that embed.
  while (activeMonitors.size >= MAX_CONCURRENT_MONITORS) {
    const oldest = activeMonitors.values().next().value;
    if (!oldest) break;
    oldest.stop();
  }
  activeMonitors.add(control);

  return control;
}

// --- qurl send pipeline (back-half shared by /qurl send + /qurl map) ---
// TODO(#55): Split commands.js into focused modules — see https://github.com/layervai/qurl-integrations/issues/55
//
// REVIEW NOTE: The size of commands.js is tracked as a follow-up in
// issue #55 and is OUT OF SCOPE for this PR. Reviewers (human or bot)
// should NOT re-flag this file's length — the split will land in its
// own PR against a stable baseline.

/**
 * Mint one-time links across a stream of connector resources, each capped at
 * TOKENS_PER_RESOURCE tokens. Legacy connectors can mint the whole resource
 * pool in one request; meta-seal connectors may reject that with a cap-1
 * render-at-mint gate, in which case this falls back to one link per request
 * for the rest of the send. When a resource is exhausted, the caller's
 * `reuploadFn` is invoked to produce a new one.
 *
 * Centralizes the re-upload / batching / quota logic so a fix lands in one
 * place across the send pipeline (file/location) and handleAddRecipients
 * (file/location).
 *
 * @param {object} opts
 * @param {string} opts.initialResourceId — resource_id from the first upload
 * @param {() => Promise<{resource_id: string}>} opts.reuploadFn — called when
 *   the current resource's token pool is drained. Must return a fresh resource.
 * @param {string} opts.expiresAt — ISO string; forwarded to mintLinks.
 * @param {number} opts.recipientCount — number of tokens to mint in total.
 * @param {string} opts.apiKey — QURL API key.
 * @param {?number} [opts.selfDestructSeconds] — when set, forwarded to mintLinks
 *   as `session_duration`. Each minted token gets an L7 session window matching
 *   the fileviewer's client-side self-destruct timer. null/undefined inherits
 *   qurl-service's QURL_SESSION_TTL default. See mintLinks for value mapping
 *   (0.5 → "1s", N>=1 → ceil to whole seconds).
 * @param {?string} [opts.guildId] — Discord guild snowflake. Forwarded to
 *   mintLinks as `guild_id` on EVERY batch so the connector can guild-scope
 *   a future watermark-attribution lookup to the minting guild (#1101).
 *   Threaded through here (not passed to mintLinks at each call site) because
 *   mintLinks is only reached via this batcher on the real send paths.
 *   Optional/back-compat — omitting it leaves the mint body unchanged.
 * @returns {Array<{qurl_link: string, qurl_id: string, resourceId: string}>}
 */
async function mintLinksInBatches({ initialResourceId, reuploadFn, expiresAt, recipientCount, apiKey, selfDestructSeconds = null, guildId }) {
  const allLinks = [];
  let currentResourceId = initialResourceId;
  // Snapshot per send so a mid-send global downgrade cannot reorder the
  // recipient->link mapping. Concurrent cold sends may each probe once; later
  // sends in this worker start at the observed cap.
  let linksPerRequest = connectorMintLinksPerRequest;
  let tokensUsed = 0;

  // Keep fallback cap-1 mints serial. Parallel calls against one resource
  // would need order-restoration and could race the per-resource token pool;
  // sequential mints preserve the recipient->link mapping and fail closed on a
  // later 5xx instead of reminting an already-created prefix.
  for (let i = 0; i < recipientCount;) {
    if (tokensUsed >= TOKENS_PER_RESOURCE) {
      const re = await reuploadFn();
      currentResourceId = re.resource_id;
      tokensUsed = 0;
    }
    const batchSize = Math.min(linksPerRequest, TOKENS_PER_RESOURCE - tokensUsed, recipientCount - i);
    // Future-proof invariant: today's operands are all >= 1 here, but a
    // later config-driven cap should fail closed instead of spinning.
    if (batchSize <= 0) {
      throw new Error('Internal mint batch invariant violated: batchSize must be positive');
    }
    let minted;
    try {
      minted = await mintLinks(currentResourceId, {
        expiresAt,
        n: batchSize,
        apiKey,
        selfDestructSeconds,
        guildId,
      });
    } catch (err) {
      if (batchSize > 1 && err?.apiCode === 'batch_cap_exceeded') {
        // The connector validates this cap before minting, so the rejected
        // probe consumes zero tokens; retry the same resource/recipient index.
        connectorMintLinksPerRequest = 1;
        linksPerRequest = 1;
        continue;
      }
      throw err;
    }
    for (const link of minted) {
      // qurl_id is the join key against qurl.accessed webhooks; empty
      // string degrades the whole monitor to bare base-msg.
      allLinks.push({ qurl_link: link.qurl_link, qurl_id: link.qurl_id || '', resourceId: currentResourceId });
    }
    tokensUsed += batchSize;
    i += batchSize;
  }
  return allLinks;
}

// executeSendPipeline — back-half of the qurl send lifecycle, shared
// by `/qurl send` and `/qurl map` after the user clicks the Send
// button on the confirm card. The destructure signature is the
// authoritative param surface; the notes below capture only non-obvious
// contract guarantees that a reader couldn't infer from the call sites
// alone.
//
// Entry gates (fire-and-forget cancel-edit + throw):
//   - `attachment.url` re-validated against `isAllowedSourceUrl`
//     when `resourceType === RESOURCE_TYPES.FILE`.
//   - `expiresIn` must be a key of `EXPIRY_LABELS`.
//   - `personalMessage` must be `null` or `string`.
//   - `recipients` must be a non-empty array (element shape is
//     `{ id: <discord-user-id>, username: <string> }`; the gate
//     does NOT validate elements — only the array's outer shape +
//     length ≤ `config.QURL_SEND_MAX_RECIPIENTS`).
// Each gate clears the caller's stale ephemeral with a cancel-
// edit then throws — the user still sees the outer catch's
// generic followUp, but the gate's specific cancel replaces
// the stale "Preparing send..." rather than co-existing with it.
//
// Caller contract (non-obvious bits — what the signature can't say):
//
//   - `attachment.url` re-validated at entry against
//     `isAllowedSourceUrl` — defense-in-depth, but callers
//     SHOULD still validate at their boundary (the entry gate
//     is the second line, not the first). A tampered persisted
//     payload reaching the gate produces a generic "Internal
//     error" cancel without leaking the SSRF detail.
//
//   - `recipients` is MUTATED in-place on the Add Recipients post-
//     send branch (deduped push of new IDs). Treat as transferred
//     ownership for the lifetime of the call, not as a snapshot.
//     A caller that reuses the same array across retries will see
//     silent double-adds.
//
// Required `interaction` surface (fail loudly at the corresponding
// call site if missing):
//
//   - `interaction.user.id`            (cooldown key + senderDiscordId
//                                       on every DB row + audit event)
//   - `interaction.channelId`          (qurl_sends.channel_id)
//   - `interaction.member?.displayName` + `user.username` fallback
//                                      (resolveSenderAlias → DM embed)
//   - `interaction.guild?.members?.fetch` (best-effort, recipient-
//                                       alias resolution for cold cache)
//   - `interaction.editReply`          (every user-visible status
//                                       update on the primary message)
//
// Resolved value is unused — both terminal completion and the
// early-exit `return interaction.editReply(...)` paths discard
// observable result; the function exists for its user-visible
// side-effects (editReply + DM fan-out).
// Bounded value-renderer for entry-gate throw messages. `String()` is
// wrapped because a hostile @@toPrimitive / toString would otherwise
// replace the gate's intended TypeError with the renderer's opaque
// throw. Code-point (not code-unit) slicing avoids splitting a
// surrogate pair across the `…` boundary.
function truncForLog(v) {
  let s;
  try {
    s = String(v);
  } catch {
    return '<unrepresentable>';
  }
  // Pre-slice bounds the [...head] spread cost on pathological inputs
  // (1MB string → 128-element array, not 1M). `truncated` is the
  // authoritative truncation signal because 65 surrogate pairs (130
  // code units) yields head-cps.length === 64 — relying on cps.length
  // alone would silently drop the `…` on that shape.
  const truncated = s.length > 128;
  const head = truncated ? s.slice(0, 128) : s;
  const cps = [...head];
  if (truncated || cps.length > 64) {
    return `${cps.slice(0, 64).join('')}…`;
  }
  return s;
}

async function executeSendPipeline(interaction, {
  apiKey,
  resourceType,
  attachment,
  locationUrl,
  locationName,
  // MUTATES: the Add Recipients post-send branch dedupe-pushes into
  // this array (see docstring's "transferred ownership" note). The
  // initial `recipients` is already deduped + bot-filtered by
  // partitionRecipients (see contract pin at commands.js:~3552):
  // upstream owns dedup, this function does NOT re-dedup the initial set.
  // Sender CAN appear in `recipients` — self-send is supported.
  recipients,
  expiresIn,
  selfDestructSeconds,
  personalMessage,
  sendNonce,
  // 'picker' | 'voice' | 'everyone'. Drives the post-send channel
  // notification (voice + everyone modes only). Optional — defaults
  // to RECIPIENT_MODE_PICKER via normalizeRecipientMode for legacy
  // callers and the picker happy path.
  recipientMode,
}) {
  // Shared cancel-edit for every entry gate. Fire-and-forget — the
  // throw is the load-bearing signal (test pins + logger.error in
  // handleCommand's outer catch). The outer catch will still append
  // a generic followUp; this edit replaces the caller's stale
  // ephemeral so the user sees a specific cancel alongside the
  // generic followUp, not a stale "Preparing send..." alongside it.
  const cancelEdit = () => interaction.editReply({
    content: '❌ Internal error — send cancelled. Please rerun the command.',
    components: [],
  }).catch(logIgnoredDiscordErr);

  // Nested so it closes over `interaction` + `cancelEdit` without
  // parameter sprawl at the call sites. The `@returns {never}`
  // annotation lets static analysis treat the call as terminating.
  /**
   * @param {new (msg: string) => Error} ErrorCtor
   * @param {string} msg
   * @returns {never}
   */
  function failGate(ErrorCtor, msg) {
    clearCooldown(interaction.user.id);
    cancelEdit();
    throw new ErrorCtor(msg);
  }

  // Defense-in-depth SSRF re-check. `/qurl send`'s front-half
  // validates attachment.url against isAllowedSourceUrl BEFORE
  // calling the pipeline; this gate catches a future caller that
  // forgets.
  // FILE-only because location sends carry no attacker-controlled
  // URL (locationUrl is built from sanitized user-text via
  // google.com/maps/search). logger.warn fires before failGate so
  // the breadcrumb lands even if cancelEdit later throws.
  if (resourceType === RESOURCE_TYPES.FILE) {
    if (!attachment || typeof attachment.url !== 'string' || !isAllowedSourceUrl(attachment.url)) {
      logger.warn('executeSendPipeline: attachment.url failed isAllowedSourceUrl gate', {
        user_id: interaction.user.id,
        send_nonce: sendNonce,
        host: attachment?.url && safeUrlHost(attachment.url),
      });
      failGate(Error, `executeSendPipeline: attachment.url failed SSRF re-validation (resourceType=${String(resourceType)})`);
    }
  }

  // `expiresIn` allowed-set gate. Today only the confirm card's
  // expiry-select dropdown can supply it, so the set is closed.
  // A future caller reconstructing from a persisted payload could
  // ship an off-set value (`'25h'`, `'bogus'`) that lands in the
  // DB and trips downstream when `expiryToISO` / `expiryToMs`
  // hit it. Validate at the boundary instead.
  if (!isValidExpiry(expiresIn)) {
    failGate(TypeError, `executeSendPipeline: expiresIn must be one of ${Object.keys(EXPIRY_LABELS).join('|')} (got ${truncForLog(expiresIn)})`);
  }

  // `personalMessage` shape gate. The DM-embed renderer expects
  // null OR string. A non-string object would silently stringify
  // to `[object Object]` in the recipient's DM — exact silent-
  // regression shape the other gates fence. Intentionally renders
  // only `typeof`, NOT the value (no truncForLog), since a string
  // here would be user-authored DM prose; we don't want even a
  // truncated 64-char prefix of that landing in prod logs.
  if (personalMessage !== null && typeof personalMessage !== 'string') {
    failGate(TypeError, `executeSendPipeline: personalMessage must be null or string (got typeof=${typeof personalMessage})`);
  }

  // `recipients` shape + cap gates. The docstring's "non-empty,
  // ≤ QURL_SEND_MAX_RECIPIENTS" contract is enforced by the `/qurl send`
  // + `/qurl map` front-half today; this is defense-in-depth for a
  // future caller (deserialized payload, programmatic retry, admin tool)
  // that skips those checks. Trips here would otherwise surface deep
  // inside mintLinksInBatches as "Failed to create any links" with no
  // caller-side breadcrumb. The non-empty check ALSO fences the chain
  // of `recipients.length` reads that follow (the cap check below, then
  // the editReply) — a non-array reaching either site would crash on
  // `.length` lookup against undefined.
  if (!Array.isArray(recipients) || recipients.length === 0) {
    // Canonical `typeof=`, `value=` rendering matches the other entry
    // gates so a prod-log reader sees one shape across the file. Empty-
    // array case renders the literal `<empty array>` sentinel
    // (truncForLog on `[]` would `String()` to the empty string, which
    // a prod-log reader couldn't distinguish from a missing value-field).
    const detail = Array.isArray(recipients)
      ? 'typeof=object, value=<empty array>'
      : `typeof=${typeof recipients}, value=${truncForLog(recipients)}`;
    failGate(TypeError, `executeSendPipeline: recipients must be a non-empty array (got ${detail})`);
  }
  if (recipients.length > config.QURL_SEND_MAX_RECIPIENTS) {
    failGate(RangeError, `executeSendPipeline: recipients.length (${recipients.length}) exceeds QURL_SEND_MAX_RECIPIENTS (${config.QURL_SEND_MAX_RECIPIENTS})`);
  }
  // Operator-visibility hook for big sends. Fires when a single send
  // crosses `largeSendThreshold()` so the operational cost (qurl-
  // service re-uploads + DM fan-out duration at Discord's per-bot
  // rate limit) surfaces in logs before it shows up on a rate-limit
  // dashboard. Sender + guild ids + resourceType are the natural
  // pivots for "which guild kicked off this 5k-recipient file send."
  const sendThreshold = largeSendThreshold();
  if (recipients.length >= sendThreshold) {
    logger.warn('Large recipient send initiated', {
      recipient_count: recipients.length,
      threshold: sendThreshold,
      cap: config.QURL_SEND_MAX_RECIPIENTS,
      resource_type: resourceType,
      user_id: interaction.user.id,
      guild_id: interaction.guildId,
    });
  }

  await interaction.editReply({ content: `Preparing links for ${recipients.length} recipient(s)...`, components: [] }).catch(logIgnoredDiscordErr);

  const sendId = crypto.randomUUID();
  let qurlLinks = [];
  let connectorResourceId = null;

  // Track whether this send has claimed the file-concurrency slot so the
  // outer finally can release it exactly once even if we hit an error path.
  let fileSendSlotClaimed = false;
  // Safety watchdog: if an upload hangs past the deadline (network stuck,
  // misbehaving upstream AbortSignal), forcibly release the slot so a bad
  // upload can never permanently consume one of the MAX_CONCURRENT_FILE_SENDS
  // slots. 5 min is generous — every internal fetch already has its own
  // shorter AbortSignal timeout, so this fires only on truly stuck flows.
  let slotWatchdog = null;
  const releaseSlot = () => {
    if (fileSendSlotClaimed) {
      activeFileSends--;
      fileSendSlotClaimed = false;
    }
    if (slotWatchdog) { clearTimeout(slotWatchdog); slotWatchdog = null; }
  };
  try {
    if (resourceType === RESOURCE_TYPES.FILE) {
      // Atomic claim. The earlier cap-check at file acceptance is UX-only
      // (told the user upfront the system was busy); five users could each
      // pass that check while activeFileSends == 0, then all sit in the
      // 3-min Step-3 form loop, then all increment here past the cap. Re-
      // check at the increment point to keep the cap honest.
      if (activeFileSends >= MAX_CONCURRENT_FILE_SENDS) {
        clearCooldown(interaction.user.id);
        logger.warn('File send rejected at slot-claim: concurrency cap reached', {
          activeFileSends, sendId, sendNonce, userId: interaction.user.id,
        });
        return interaction.editReply({
          content: 'The bot is processing too many file sends right now. Please try again in a moment.',
          components: [],
        });
      }
      activeFileSends++;
      fileSendSlotClaimed = true;
      slotWatchdog = setTimeout(() => {
        if (fileSendSlotClaimed) {
          logger.error('activeFileSends slot watchdog fired — slot force-released', { sendId, sendNonce, userId: interaction.user.id });
          activeFileSends--;
          fileSendSlotClaimed = false;
        }
      }, 5 * 60 * 1000);
      slotWatchdog.unref();
      const filename = sanitizeFilename(attachment.name);
      const expiresAt = expiryToISO(expiresIn);

      // Download once, cache the buffer for re-uploads.
      // selfDestructSeconds threads through both initial upload AND every
      // re-upload, so the bot's "Add Recipients" mints the same TTL on
      // each new resource that gets registered (TOKENS_PER_RESOURCE
      // exhaustion → re-upload → new connector resource).
      const firstUpload = await downloadAndUpload(attachment.url, filename, attachment.contentType, apiKey, selfDestructSeconds);
      connectorResourceId = firstUpload.resource_id;
      // Use a holder so we can null out the reference after all re-uploads
      // finish — the subsequent link-monitor closure would otherwise pin up
      // to 25MB in memory for up to an hour per concurrent send.
      const bufHolder = { buf: firstUpload.fileBuffer };

      // try/finally around mintLinksInBatches so a throw inside batching
      // still releases the 25 MB buffer — otherwise the reuploadFn closure
      // would pin it on the activeMonitors set / pending error handler for
      // up to an hour per concurrent send under GC pressure.
      let allLinks;
      try {
        allLinks = await mintLinksInBatches({
          initialResourceId: firstUpload.resource_id,
          reuploadFn: () => reUploadBuffer(bufHolder.buf, filename, attachment.contentType, apiKey, selfDestructSeconds),
          expiresAt,
          recipientCount: recipients.length,
          apiKey,
          selfDestructSeconds,
          // Guild-scope the mint so watermark attribution (/qurl detect,
          // #1101) can resolve back to this guild. interaction.guildId is
          // guaranteed non-null here — the /qurl send + /qurl map entry
          // points both DM-reject before reaching the pipeline.
          guildId: interaction.guildId,
        });
      } finally {
        bufHolder.buf = null;
      }

      if (allLinks.length < recipients.length) {
        logger.error('mintLinks returned fewer links than expected', { expected: recipients.length, got: allLinks.length });
        clearCooldown(interaction.user.id);
        return interaction.editReply({ content: `Only ${allLinks.length} of ${recipients.length} links could be created. Please try again.` });
      }

      qurlLinks = recipients.map((r, i) => ({
        recipientId: r.id,
        qurlLink: allLinks[i].qurl_link,
        qurlId: allLinks[i].qurl_id,
        resourceId: allLinks[i].resourceId,
      }));
      logger.audit(AUDIT_EVENTS.UPLOAD_SUCCESS, { send_id: sendId, kind: 'file' });
    } else {
      // Location send — upload JSON payload to connector, then mint links
      // in batches, downgrading to one-per-request if the connector reports
      // the meta-seal cap, and re-upload when the resource pool is drained.
      const locPayload = { type: 'google-map', url: locationUrl, name: locationName || locationUrl };
      // Note: google-map JSON resources hit the connector's render
      // carve-out (mapEmbedTmpl/mapFallbackTmpl don't honor
      // expire_after at view time — qurl-integrations-infra#480).
      // We still forward selfDestructSeconds so behavior matches the
      // contract once the carve-out is removed; today it's a no-op.
      const firstUpload = await uploadJsonToConnector(locPayload, 'location.json', apiKey, selfDestructSeconds);
      connectorResourceId = firstUpload.resource_id;

      const expiresAt = expiryToISO(expiresIn);
      const allLinks = await mintLinksInBatches({
        initialResourceId: firstUpload.resource_id,
        reuploadFn: () => uploadJsonToConnector(locPayload, 'location.json', apiKey, selfDestructSeconds),
        expiresAt,
        recipientCount: recipients.length,
        apiKey,
        selfDestructSeconds,
        // Guild-scope the mint for watermark attribution (#1101) — see the
        // file-send branch above. Maps payloads carry no image to watermark
        // today, but threading guild_id keeps the two pipelines symmetric
        // and future-proofs a watermarked map render.
        guildId: interaction.guildId,
      });

      if (allLinks.length < recipients.length) {
        logger.error('mintLinks returned fewer links than expected for location', { expected: recipients.length, got: allLinks.length });
        clearCooldown(interaction.user.id);
        return interaction.editReply({ content: `Only ${allLinks.length} of ${recipients.length} links could be created. Please try again.` });
      }

      qurlLinks = recipients.map((r, i) => ({
        recipientId: r.id,
        qurlLink: allLinks[i].qurl_link,
        qurlId: allLinks[i].qurl_id,
        resourceId: allLinks[i].resourceId,
      }));
      logger.audit(AUDIT_EVENTS.UPLOAD_SUCCESS, { send_id: sendId, kind: 'location' });
    }
  } catch (error) {
    // Audit for the CloudWatch metric filter + alarm at qurl-integrations-infra
    // qurl-bot-discord/terraform/monitoring.tf (qurl-integrations#276); the why
    // lives in the QURL_SEND_CREATE_LINK_FAILURE docstring in constants.js.
    // kindMap (not a `=== FILE ? 'file' : 'location'` ternary) so a future third
    // RESOURCE_TYPES surfaces as `null` — discoverable in CloudWatch — rather than
    // silently bucketing as 'location'. quota_exceeded skip lives in emitMintFailureAudit.
    const kindMap = { [RESOURCE_TYPES.FILE]: 'file', [RESOURCE_TYPES.MAPS]: 'location' };
    emitMintFailureAudit(error, { sendId, kind: kindMap[resourceType] ?? null });
    logger.error('Failed to prepare QURL links', {
      error: error.message,
      apiCode: error.apiCode,
      status: error.status,
      sendId,
    });
    clearCooldown(interaction.user.id); // allow retry on failure
    // Slot release lives in the `finally` block below — it ALWAYS runs
    // after a return-from-catch, and `releaseSlot` is idempotent via
    // the `fileSendSlotClaimed` flag. Dropping the duplicate call here
    // keeps the single-release-path invariant visible at a glance.
    // Surface a specific message for known upstream failure codes so the
    // user knows what to do (re-upload to refresh the per-resource quota)
    // instead of seeing a generic "try again" that won't help.
    if (error.apiCode === 'quota_exceeded') {
      const isFile = resourceType === RESOURCE_TYPES.FILE;
      const verb = isFile ? 're-upload the file' : 'edit the location query and resend';
      return interaction.editReply({
        content: `Couldn't create more links — this ${isFile ? 'file' : 'location'} has hit its share limit (${TOKENS_PER_RESOURCE} per upload). To send to more recipients, ${verb}.`,
      });
    }
    return interaction.editReply({ content: 'Failed to create links. Please try again.' });
  } finally {
    // Release the file-concurrency slot as soon as the mint+batch phase is
    // done — we no longer hold the 25 MB buffer past this point.
    releaseSlot();
  }

  if (qurlLinks.length === 0) {
    clearCooldown(interaction.user.id);
    return interaction.editReply({ content: 'Failed to create any links. Please try again.' });
  }

  // Compute the absolute expiry instant once for this dispatch (Unix
  // seconds — Discord's <t:N:R> format requires seconds, not millis).
  // Using send-time + duration rather than reading from the API mint
  // response since `mintLinks` doesn't currently surface `expires_at`.
  // Drift between this clock and the API's enforcement clock is the
  // wall-clock gap between THIS compute site and the earlier mint call
  // (mintLinksInBatches has already returned by the time we land here
  // on the send-pipeline path; on handleAddRecipients the gap also
  // covers re-download + re-upload + re-mint). So recipients see
  // "in 24 hours" measured against send-time, not mint-time. Negligible
  // at the 30m–7d horizon — even a worst-case 10s gap rounds the same
  // way in the relative-time display.
  //
  // Computed BEFORE recordQURLSendBatch so a malformed `expiresIn`
  // throwing in `expiryToMs` aborts the dispatch BEFORE any DDB rows
  // are written — otherwise the DB writes would happen, then the throw
  // would bubble out with no DMs sent, leaving orphan QURL records
  // with no recipient. The throw fires HERE (at the hoisted compute
  // site below, function-scoped) rather than at the per-recipient
  // render step inside batchSettled — that distinction matters for
  // the failure-mode bookkeeping: a thrown expiryToMs surfaces as a
  // single uncaught error at the function level, not as N
  // `DISPATCH_FAILED` audit emissions. Closes #352.
  const expiresAt = Math.floor((Date.now() + expiryToMs(expiresIn)) / 1000);

  // Persist ALL links to DB BEFORE sending DMs. If the write fails the links
  // still exist on the QURL side but there's no local record to revoke them
  // later — abort the send and surface the error instead of continuing to DMs.
  try {
    await db.recordQURLSendBatch(qurlLinks.map(link => ({
      sendId, senderDiscordId: interaction.user.id, recipientDiscordId: link.recipientId,
      resourceId: link.resourceId, resourceType, qurlLink: link.qurlLink,
      // qurl_id is the GSI lookup key the qurl.expired webhook handler
      // uses to find the recipient row for DM editing. Pulled from
      // mintLinksInBatches return value above. Sparse on the write
      // side — see recordQURLSendBatch.
      qurlId: link.qurlId,
      // CONTRACT: targetType is always 'user' for new rows post-PR
      // #313 (the only caller is executeSendPipeline via the confirm
      // card on /qurl send + /qurl map, both of which DM individual
      // recipients). The column is kept because the revoke-list
      // renderer's branch on `s.target_type` still has to handle
      // historical legacy /qurl send wizard rows ('channel') during the TTL drain
      // window. #318 drops the formatRevokeLabel non-'user' branch
      // once no revoke-visible row has `target_type !== 'user'` — the
      // drain happens naturally as the revoke renderer filters on
      // `expires_at`, so the gate is condition-driven (no live legacy
      // rows surface in /qurl revoke) rather than date-driven. The
      // EXPIRY_LABELS max window (7d) bounds when that condition is
      // safe to assume post-deploy. No entry-gate fires because the
      // value can't drift — it's a literal, not a forwarded param.
      expiresIn, channelId: interaction.channelId, targetType: 'user',
      // guild_id persisted on each row so /qurl detect (#1101) can
      // guild-scope watermark attribution at read time (defense-in-depth
      // alongside the connector's own X-Guild-Id scoping). Mirrors the
      // guildId threaded into the mint above. Non-null here (entry points
      // DM-reject). Sparse write — see recordQURLSendBatch.
      guildId: interaction.guildId,
    })));
  } catch (err) {
    // Log the orphaned QURL resources at error level so an operator can
    // manually revoke them — they exist on the QURL side with no local row.
    logger.error('recordQURLSendBatch failed; aborting send to keep state consistent', {
      sendId, error: err.message, linkCount: qurlLinks.length,
      orphanedResources: qurlLinks.map(l => ({ resourceId: l.resourceId, qurlLink: l.qurlLink })),
    });
    clearCooldown(interaction.user.id);
    return interaction.editReply({
      content: 'Failed to save link records. Links were not sent. Please try again.',
    });
  }

  // Send DMs. `expiresAt` is Unix seconds, computed above the DDB
  // write — see the #352 hoist comment near recordQURLSendBatch for
  // the clock-drift caveat that was previously documented here.
  let delivered = 0;
  let failed = 0;
  const failedUsers = [];
  const recipientMap = new Map(recipients.map(r => [r.id, r]));

  // Mirror handleAddRecipients: hoist resolveSenderAlias out of the
  // per-recipient batchSettled callback. The function is a pure
  // resolution of nickname > globalName > username from `interaction`,
  // so the per-link call inside the callback was N redundant
  // resolutions of the same string. Both pipelines now share the same
  // hoist pattern for both expiresAt AND senderAlias.
  const senderAlias = resolveSenderAlias(interaction);
  const { guildName, guildIconUrl } = resolveGuildProvenance(interaction);

  const dmResults = await batchSettled(qurlLinks, async (link) => {
    const recipient = recipientMap.get(link.recipientId);
    // Audit in `finally` so the metric fires for every recipient regardless
    // of where the dispatch fails — sendDM resolving to false, sendDM
    // throwing (against contract — see apps/discord/src/discord.js), OR
    // buildDeliveryPayload throwing (e.g. on a non-integer expiresAt —
    // see its `Number.isInteger` guard; would throw on every iteration
    // of the batch, since expiresAt is computed once above).
    // Audit fires BEFORE the DB write so a DDB-layer throw can't suppress
    // it either — that's the failure mode the audit metric exists to
    // measure. Coverage spans the entire dispatch attempt, not just the
    // network leg.
    let result = { ok: false };
    try {
      const dmPayload = buildDeliveryPayload({
        senderAlias,
        guildName,
        guildIconUrl,
        qurlLink: link.qurlLink,
        expiresAt,
        personalMessage,
      });
      result = await sendDM(link.recipientId, dmPayload);
    } finally {
      logger.audit(result.ok === true ? AUDIT_EVENTS.DISPATCH_SENT : AUDIT_EVENTS.DISPATCH_FAILED, { send_id: sendId });
    }
    await persistDispatchResult(sendId, link.recipientId, result);
    return { recipientId: link.recipientId, username: recipient?.username, sent: result.ok === true };
  }, 5);

  for (const r of dmResults) {
    if (r.status === 'fulfilled' && r.value.sent) {
      delivered++;
    } else {
      failed++;
      if (r.status === 'fulfilled') {
        failedUsers.push({ id: r.value.recipientId, username: r.value.username });
      } else {
        failedUsers.push({ id: null, username: 'unknown' });
      }
    }
  }

  // Save send config for "Add Recipients" reuse. For file sends, also stash
  // the Discord CDN URL + content type so we can re-download + re-upload when
  // adding recipients (the original resource's 10-token pool may be drained).
  // Logged-and-swallowed: a failure here doesn't block DM delivery (already
  // done above) but it disables future Add Recipients / revoke-via-ui for
  // this send. The per-link rows in qurl_sends persisted above, so /qurl
  // revoke by sendId still works even if this row is missing.
  try {
    await db.saveSendConfig({
      sendId, senderDiscordId: interaction.user.id, resourceType, connectorResourceId,
      actualUrl: locationUrl || null, expiresIn, personalMessage, locationName,
      attachmentName: attachment?.name || null,
      attachmentContentType: attachment?.contentType || null,
      attachmentUrl: attachment?.url || null,
      selfDestructSeconds,
    });
  } catch (err) {
    logger.error('saveSendConfig failed; Add Recipients will be unavailable for this send', {
      sendId, error: err.message,
    });
  }

  // Ephemeral confirmation with Add Recipients + Revoke buttons.
  // Match on the Discord id (snowflake, globally unique) rather than the
  // display username — usernames can collide within a guild.
  // Shares REVOKE_TRUNC_LIMIT (module scope) so the send-confirmation
  // "Recipients: …" line and the post-revoke "Revoked for: …" line
  // truncate at the same threshold.
  const failedUserIds = new Set(failedUsers.map(u => u.id || u));
  const successNames = recipients.filter(r => !failedUserIds.has(r.id)).map(r => resolveRecipientAlias(r, interaction));

  // Plain-form failed names (sanitizeDisplayNamePlain is already
  // applied inside resolveRecipientAlias). Used for the attachment
  // file; message-content rendering escapes per name.
  const failedNamesPlain = failedUsers.map(u => resolveRecipientAlias(u, interaction));
  const buildConfirmMsg = (showAll) => renderSendConfirm({
    delivered, expiresIn, selfDestructSeconds,
    failedNamesPlain, successNames, showAll,
  });

  const confirmRendered = buildConfirmMsg(false);
  let confirmMsg = confirmRendered.content;
  // In attachment mode the file IS the full list, so suppress the
  // Show Recipients toggle — the same shape the post-revoke flow uses.
  const needsExpand = confirmRendered.needsExpand;

  const buttonRow = new ActionRowBuilder().addComponents(
    new ButtonBuilder()
      .setCustomId(`qurl_add_${sendId}`)
      .setLabel('Add Recipients')
      .setStyle(ButtonStyle.Primary),
    new ButtonBuilder()
      .setCustomId(`qurl_revoke_${sendId}`)
      .setLabel('Revoke All')
      .setStyle(ButtonStyle.Danger),
  );
  if (needsExpand) {
    buttonRow.addComponents(
      new ButtonBuilder()
        .setCustomId(`qurl_expand_${sendId}`)
        .setLabel('Show Recipients')
        .setStyle(ButtonStyle.Secondary),
    );
  }

  // When successNames + failedNames overflow Discord's 2000-char content
  // cap, attach the full lists as `recipients.txt` on this initial
  // editReply. Subsequent monitor ticks / Add Recipients edits don't
  // pass `files`, so Discord keeps the existing attachment without
  // re-uploading. Add Recipients does NOT regenerate the attachment;
  // the newly-added users surface in the post-revoke "Revoked for: …"
  // list (with its own overflow path) and in monitor link-status
  // updates.
  // Hoisted above editReply so the initial confirmation can render the
  // `👀 0 of N viewed` baseline via monitor.getFullMsg(); without the
  // hoist the sender sees no counter until the first poll + at least
  // one view.
  let monitor = null;
  if (delivered > 0) {
    monitor = monitorLinkStatus(sendId, interaction, qurlLinks, recipients, expiresIn, confirmMsg, buttonRow, delivered);
  }
  const initialPayload = {
    content: monitor ? monitor.getFullMsg() : confirmMsg,
    components: delivered > 0 ? [buttonRow] : [],
  };
  if (confirmRendered.attachmentText) {
    initialPayload.files = [
      new AttachmentBuilder(Buffer.from(confirmRendered.attachmentText, 'utf8'), { name: 'recipients.txt' }),
    ];
  }
  let response;
  try {
    response = await interaction.editReply(initialPayload);
  } catch (err) {
    // Hoisted monitor would otherwise tick for an hour with a dead
    // interaction handle, calling editReply 30+ times that all fail.
    if (monitor) monitor.stop();
    throw err;
  }

  // Arm the cross-replica view-counter fast-path. The webhook receiver
  // (routes/qurl-webhook.js) edits THIS ephemeral confirmation from any
  // replica using the persisted interaction token, so a view lands on
  // the sender's "👀 N viewed" within sub-second latency regardless of
  // which replica hosts the in-memory monitor. Persisted only AFTER the
  // editReply landed: before that, the @original message doesn't exist
  // for the fast-path's PATCH to target, and confirmMsg/delivered exist
  // only here (saveSendConfig ran earlier, before they were computed).
  //
  // Gate: delivered > 0 (no monitor / no counter otherwise) AND a token
  // is present AND the send is NOT view-counter-degraded. Best-effort +
  // logged-swallowed exactly like saveSendConfig — a failure here just
  // leaves the fast-path inert (the poll backstop still renders), it must
  // not break a send whose DMs already delivered.
  //
  // DEGRADED GATE: when any link is missing its qurl_id the monitor
  // suppresses the counter entirely (renders bare baseMsg — a partial-
  // attribution "N viewed" would mislead worse than no counter). The
  // fast-path renders from qurl_views and CANNOT see that degrade (it
  // hardcodes degraded:false), so arming it on a degraded send would let
  // a view stamp the very partial counter the monitor suppresses, then
  // the poll would flip it back to bare — a forbidden flicker. So we
  // simply DON'T arm the fast-path on a degraded send; its (bare) poll
  // render is the sole renderer, matching today's behavior. Mirrors the
  // monitor's own construction-time degrade check.
  //
  // SECURITY: interaction.token is a live bearer cred — NEVER log it. The
  // catch below logs only sendId + err.message, never the token.
  // applicationId is the standard discord.js Interaction property (set on
  // the reconstructed worker interaction too, from the raw gateway
  // application_id); no `client` fallback needed.
  const counterDegraded = qurlLinks.some(l => !l.qurlId);
  if (delivered > 0 && interaction.token && !counterDegraded) {
    try {
      await db.saveSendConfirmState(sendId, {
        interactionToken: interaction.token,
        interactionAppId: interaction.applicationId,
        expectedCount: delivered,
        // The COLLAPSED base — exactly the string the monitor uses as
        // baseMsg, WITHOUT the "👀 …" counter line (confirmMsg is that
        // base; monitor.getFullMsg() would double-stamp the counter).
        confirmBaseMsg: confirmMsg,
        // Optional inline qurl_id fallback cache. saveSendConfirmState
        // caps large sends to [] before writing so qurl_send_configs
        // never approaches DDB's item-size limit; the normal path renders
        // from the sharded aggregate, and rare fallback reads recipient
        // rows via getSendItems. Filter falsy — legacy/non-guild links may
        // omit qurlId.
        confirmQurlIds: qurlLinks.map(l => l.qurlId).filter(Boolean),
        viewedCount: 0,
        // Epoch seconds. Aligned to the real Discord interaction-token TTL
        // (~15 min), NOT a minute past it: getSendRenderState self-defends
        // by treating a past value as absent, so setting this to ~15 min
        // makes the fast-path stop trusting the token right when Discord
        // kills it — closing the dead-token retry window where a view
        // between token-death and self-defense would fire a PATCH that
        // 401s. Kept ABOVE the 14-min monitor cap so the fast-path stays
        // live for the whole window the token is actually valid (a value
        // ≤14 min would self-defend while the monitor + token are still
        // good). This also bounds the sensitive token's at-rest life until
        // the qurl-bot-ddb DDB TTL on confirm_expires_at lands
        // (qurl-integrations-infra#1227) to physically reap the row.
        confirmExpiresAt: Math.floor(Date.now() / 1000) + 15 * 60,
      });
    } catch (err) {
      logger.error('saveSendConfirmState failed; webhook view-counter fast-path disabled for this send (poll backstop still renders)', {
        sendId, error: err.message,
      });
    }
  }

  // Non-ephemeral channel notification when sending to @everyone or the
  // voice-channel population. Recipients on voice or scrolling on mobile
  // miss the DM ping otherwise — this post is the only chat signal that
  // a fan-out send happened. Picker-mode skips: recipient list is
  // explicit and small, DM ping suffices. Routed via REST not
  // `interaction.channel.send()` because http-only worker mode has an
  // empty `client.channels.cache` (no GUILD_CREATE without login()), so
  // the discord.js getter resolves to null and `.send()` would throw.
  // Logged-and-continued on failure: a missing Send Messages permission
  // in a customer server must NOT fail a send whose DMs already
  // delivered. Fire-and-forget so the collector below arms in parallel
  // with the REST flush.
  const mode = normalizeRecipientMode(recipientMode);
  if (delivered > 0 && (mode === RECIPIENT_MODE_EVERYONE || mode === RECIPIENT_MODE_VOICE)) {
    // sanitizeDisplayName is load-bearing here, not in the DM embed:
    // public-channel post is a wider blast radius than a DM, so a
    // U+202E in the nickname would flip the announcement RTL for
    // every viewer.
    const safeName = sanitizeDisplayName(senderAlias);
    const audience = mode === RECIPIENT_MODE_VOICE
      ? 'everyone in this voice channel'
      : 'everyone in this server';
    const notifyMsg = `📩 **${safeName}** shared something with ${audience} via **qURL Bot** — check your DMs from qURL Bot.`;
    sendChannelMessage(interaction.channelId, { content: notifyMsg })
      .then((result) => {
        if (!result.ok) {
          logger.warn('Failed to send channel notification', {
            sendId, channelId: interaction.channelId, status: result.status, error: result.error,
          });
        }
      });
  }

  logger.info('qurl send pipeline completed', {
    sender: interaction.user.id, sendId, resourceType, delivered, failed, expiresIn,
  });

  // Collector handles multiple button clicks (Add Recipients can be clicked multiple times)
  if (monitor) {
    let addRecipientsCount = 0; // Track cumulative adds for cap enforcement
    // Running union of the send's qurl_ids (original + every /qurl add)
    // so the fast-path re-persist always writes the COMPLETE set, not
    // just the latest batch. Seeded from the original send links.
    const allQurlIds = qurlLinks.map(l => l.qurlId).filter(Boolean);
    // Single source of truth for the "adding recipients" lock: the global
    // addRecipientsLocks Set keyed by sendId. Acquire at the top of the
    // collect handler, release in a single outer finally{} so any throw
    // between acquire and the previous dual-flag release paths can't
    // permanently block subsequent button clicks.

    // Collector arms AFTER the editReply (it needs `response` as anchor).
    // On collector-setup throw we must stop the already-running monitor
    // — without that the poll loop would leak the interaction +
    // recipients + buttonRow closure for up to an hour.
    let collector;
    try {
      collector = response.createMessageComponentCollector({
        filter: (i) => i.user.id === interaction.user.id,
        time: TIMEOUTS.QURL_REVOKE_WINDOW,
      });
    } catch (err) {
      logger.error('Failed to create button collector', { sendId, error: err.message });
      monitor.stop();
      return;
    }

    let showAllRecipients = false;

    // `revokeInFlight` dedups concurrent Revoke clicks. `revokeSucceeded`
    // guards the on('end') re-render so a Failed message isn't overwritten
    // by a stale "Revoked 0/0". These flags are collector-local UX gates;
    // revokingSendLocks handles same-process cross-collector Revoke only
    // while work is active. After the lock releases, and across processes,
    // revoked_at is the correctness boundary until #862 closes the write
    // window.
    let revokeResultUserNames = [];
    let revokeResultTotal = 0;
    // Authoritative DDB strict-success count. Tracked separately from
    // `revokeResultUserNames.length` so the header stays correct even
    // if a successful recipient_id can't be name-resolved against
    // `recipients[]`.
    let revokeResultSuccess = 0;
    let revokeShowAll = false;
    let revokeInFlight = false;
    let revokeSucceeded = false;
    let revokeResultKnown = false;

    collector.on('collect', async (btnInteraction) => {
      if (btnInteraction.customId === `qurl_expand_${sendId}`) {
        await btnInteraction.deferUpdate().catch(logIgnoredDiscordErr);
        showAllRecipients = !showAllRecipients;
        // buildConfirmMsg now returns {content, attachmentText, needsExpand};
        // extract content for the monitor + editReply (string-only).
        confirmMsg = buildConfirmMsg(showAllRecipients).content;
        // The expand/collapse choice lives in the IN-MEMORY monitor only
        // (updateBaseMsg → the poll re-renders the expanded list). It is
        // deliberately NOT persisted: the webhook fast-path always renders
        // the persisted COLLAPSED base (content-only edit), so right after
        // an expand a concurrent fast-path counter edit can briefly show
        // the collapsed list — the next early poll tick (≤5s) restores the
        // expanded view off the updated in-memory base. Accepted minor
        // toggle-flicker (option c); the poll is the self-heal floor.
        monitor.updateBaseMsg(confirmMsg);
        const fullMsg = monitor.getFullMsg();
        const updatedRow = new ActionRowBuilder().addComponents(
          new ButtonBuilder().setCustomId(`qurl_add_${sendId}`).setLabel('Add Recipients').setStyle(ButtonStyle.Primary),
          new ButtonBuilder().setCustomId(`qurl_revoke_${sendId}`).setLabel('Revoke All').setStyle(ButtonStyle.Danger),
          new ButtonBuilder().setCustomId(`qurl_expand_${sendId}`).setLabel(showAllRecipients ? 'Hide Recipients' : 'Show Recipients').setStyle(ButtonStyle.Secondary),
        );
        await interaction.editReply({ content: fullMsg, components: [updatedRow] }).catch(logIgnoredDiscordErr);
        return;
      }

      if (btnInteraction.customId === `qurl_revoke_expand_${sendId}`) {
        // Toggle Show Recipients / Hide Recipients on the post-revoke list.
        await btnInteraction.deferUpdate().catch(logIgnoredDiscordErr);
        revokeShowAll = !revokeShowAll;
        const updated = renderRevokeMsg(sendId, revokeResultUserNames, revokeResultTotal, revokeShowAll, revokeResultSuccess);
        await interaction.editReply(revokeReplyPayload(updated)).catch(logIgnoredDiscordErr);
        return;
      }

      if (btnInteraction.customId === `qurl_revoke_${sendId}`) {
        // Sync dedup before any await (Node single-threaded).
        if (revokeInFlight) return btnInteraction.deferUpdate().catch(logIgnoredDiscordErr);
        if (revokingSendLocks.has(sendId)) {
          await btnInteraction.reply({ content: ALREADY_REVOKING_SEND_MSG, ephemeral: true }).catch(logIgnoredDiscordErr);
          return;
        }
        if (addRecipientsLocks.has(sendId)) {
          await btnInteraction.reply({ content: `${ADD_RECIPIENTS_IN_PROGRESS_MSG} Finish the current selection or try again in a moment.`, ephemeral: true }).catch(logIgnoredDiscordErr);
          return;
        }
        revokeInFlight = true;
        revokingSendLocks.add(sendId);
        // Keep this lock owned by the revoke work, not the collector lifetime:
        // if delete I/O hangs, Add stays blocked until that work settles (or
        // the process restarts) rather than minting while revoke may still run.
        // Stop monitor BEFORE any editReply — its poll loop can
        // overwrite the revoke-result message otherwise. Bare call
        // (no `if (monitor)`) — we're inside the `if (monitor) { ... }`
        // collector-setup block; the guard above already proved truthy.
        try {
          monitor.stop();
          await btnInteraction.deferUpdate().catch(logIgnoredDiscordErr);
          let persistedSendConfig;
          try {
            persistedSendConfig = await db.getSendConfig(sendId, interaction.user.id);
          } catch (err) {
            logger.warn('Could not pre-check send revoked state before button revoke', { sendId, error: err.message });
          }
          if (persistedSendConfig?.revoked_at) {
            // Stale collectors do not share revokeSucceeded, so persisted
            // revoked_at is their terminal gate after another collector wins.
            revokeResultUserNames = [];
            revokeResultTotal = 0;
            revokeResultSuccess = 0;
            revokeShowAll = false;
            revokeResultKnown = false;
            // Keep revokeInFlight true after success as the collector-local
            // terminal gate for duplicate Revoke clicks; revokingSendLocks only
            // covers in-progress work across same-process collectors.
            revokeSucceeded = true;
            await interaction.editReply({ content: 'Links for this send have already been revoked.', components: [] }).catch(logIgnoredDiscordErr);
            return;
          }
          await interaction.editReply({ content: 'Revoking links...', components: [] }).catch(logIgnoredDiscordErr);
          const revoked = await revokeAllLinks(sendId, interaction.user.id, apiKey, resolveSenderAlias(interaction));
          // Iterate `recipients` (canonical send-confirmation order)
          // and filter by membership — `successUserIds` walks Set
          // insertion order from resource-grouped iteration, which
          // doesn't match what the user saw on "Recipients: …".
          const successSet = new Set(revoked.successUserIds);
          revokeResultUserNames = recipients
            .filter(r => successSet.has(r.id))
            .map(r => resolveRecipientAlias(r, interaction));
          revokeResultTotal = revoked.total;
          revokeResultSuccess = revoked.success;
          revokeShowAll = false;
          revokeResultKnown = true;
          const initial = renderRevokeMsg(sendId, revokeResultUserNames, revokeResultTotal, false, revokeResultSuccess);
          await interaction.editReply(revokeReplyPayload(initial)).catch(logIgnoredDiscordErr);
          // Keep revokeInFlight true after success as the collector-local
          // terminal gate for duplicate Revoke clicks; revokingSendLocks only
          // covers in-progress work across same-process collectors.
          revokeSucceeded = true;
          // Freeze the confirmation display so a late webhook fast-path
          // edit can't resurrect a live-looking "👀 N viewed" counter
          // over this "Revoked X/Y" terminal copy (getSendRenderState
          // reads confirm_terminal → terminal, which the fast-path checks
          // FIRST and skips on). Best-effort + logged-swallowed: a miss
          // narrows to a cosmetic race (a view landing in the tens-of-ms
          // before the flag lands could re-render the counter once); the
          // monitor.stop() above already halts the poll, so this is the
          // only remaining off-monitor editor to fence.
          db.markConfirmTerminal(sendId).catch((err) => {
            logger.warn('markConfirmTerminal (revoke) failed; a late fast-path edit could briefly re-render the counter', { sendId, error: err.message });
          });
        } catch (err) {
          logger.error('Revoke failed', { sendId, error: err.message });
          await interaction.editReply({
            content: 'Failed to revoke links. Try `/qurl revoke` instead.',
            components: [],
          }).catch(logIgnoredDiscordErr);
          // Links still exist after a failed revoke, so Add Recipients can
          // reopen. Keep only successful revokes sticky.
          revokeInFlight = false;
        } finally {
          revokingSendLocks.delete(sendId);
        }
        // Collector keeps running for the post-revoke expand toggle;
        // its `time:` window auto-expires.

      } else if (btnInteraction.customId === `qurl_add_${sendId}`) {
        // =====================================================================
        // CRITICAL SECTION — every path that can reach addRecipientsLocks.add()
        // must stay synchronous until the lock is claimed. Node.js is
        // single-threaded: if check+claim happen synchronously, a second button
        // click dispatched to the same handler cannot observe the unlocked
        // state. Awaiting in rejection branches is fine because they return.
        // =====================================================================
        // Check-and-claim are now adjacent: if the flag is unset, grab it
        // FIRST (before any cap check), then verify remaining capacity and
        // release on rejection. That way a future refactor that adds an
        // `await` in the remaining check can't reopen a racy window.
        if (revokingSendLocks.has(sendId) || revokeSucceeded) {
          // Completed revokes set revoked_at before DELETE attempts, even if
          // individual deletes later fail, so stale Add clicks stay disabled.
          let content = ALREADY_REVOKING_SEND_MSG;
          if (revokeSucceeded) {
            if (!revokeResultKnown) {
              content = 'This send has already been revoked. Add Recipients is disabled.';
            } else if (revokeResultTotal === 0) {
              content = 'No live links remain for this send.';
            } else if (revokeResultSuccess < revokeResultTotal) {
              content = 'Revoke already ran for this send. Add Recipients is disabled.';
            } else {
              content = 'Links for this send have already been revoked.';
            }
          }
          await btnInteraction.reply({ content, ephemeral: true }).catch(logIgnoredDiscordErr);
          return;
        }
        if (addRecipientsLocks.has(sendId)) {
          await btnInteraction.reply({ content: `${ADD_RECIPIENTS_IN_PROGRESS_MSG} Finish the current selection or try again in a moment.`, ephemeral: true }).catch(logIgnoredDiscordErr);
          return;
        }
        addRecipientsLocks.add(sendId);
        // Single outer try/finally guarantees the lock is released on every
        // exit path — even if a synchronous throw happens between acquire
        // and any early return. The old dual-flag pattern had multiple
        // release sites and risked permanent lock-out on a missed release.
        try {
          const remaining = config.QURL_SEND_MAX_RECIPIENTS - delivered - addRecipientsCount;
          if (remaining <= 0) {
            await btnInteraction.reply({
              content: `Recipient limit reached (${config.QURL_SEND_MAX_RECIPIENTS} max).`,
              ephemeral: true,
            }).catch(logIgnoredDiscordErr);
            return;
          }
          if (isOnCooldown(interaction.user.id)) {
            await btnInteraction.reply({ content: 'Please wait before adding more recipients.', ephemeral: true }).catch(logIgnoredDiscordErr);
            return;
          }
          setCooldown(interaction.user.id);

          // Show user select menu — collect the response on the REPLY message.
          const maxSelect = Math.min(
            USER_SELECT_PER_PICK_CAP,
            remaining,
            DISCORD_SELECT_MAX_VALUES_HARD_CAP,
          );
          const userSelectRow = new ActionRowBuilder().addComponents(
            new UserSelectMenuBuilder()
              .setCustomId(`qurl_addusers_${sendId}`)
              .setPlaceholder('Select users to send to')
              .setMinValues(1)
              .setMaxValues(maxSelect)
          );
          const selectReply = await btnInteraction.reply({
            content: 'Select additional recipients:',
            components: [userSelectRow],
            ephemeral: true,
            fetchReply: true,
          });

          try {
            const selectInteraction = await selectReply.awaitMessageComponent({
              componentType: ComponentType.UserSelect,
              time: 60000,
            });

            await selectInteraction.deferUpdate();
            const addResult = await handleAddRecipients(
              sendId, selectInteraction.users, interaction, apiKey,
            );

            // Extend recipients[] for the post-revoke names line.
            // Dedupe by id — re-Add of an already-included user.
            if (addResult.newRecipients?.length) {
              const existingIds = new Set(recipients.map(r => r.id));
              for (const r of addResult.newRecipients) {
                if (!existingIds.has(r.id)) recipients.push(r);
              }
            }

            if (addResult.delivered > 0) {
              addRecipientsCount += addResult.delivered;
              monitor.addRecipients(addResult.delivered, addResult.newLinks);
              // Extend the running qurl_id union with this batch's links.
              for (const l of (addResult.newLinks || [])) {
                if (l?.qurlId) allQurlIds.push(l.qurlId);
              }
              const totalSent = delivered + addRecipientsCount;
              confirmMsg = `Sent to ${totalSent} user${totalSent !== 1 ? 's' : ''} | Expires: ${expiresIn} | ${formatSelfDestructSegment(selfDestructSeconds)}`;
              if (failed > 0) confirmMsg += `\n${failed} could not be reached`;
              monitor.updateBaseMsg(confirmMsg);
              await interaction.editReply({ content: monitor.getFullMsg(), components: [buttonRow] });
              // Mid-life degrade: an added link missing its qurl_id flips
              // the send to view-counter-degraded (the monitor renders bare
              // baseMsg from here on). The fast-path can't see that, so
              // DISARM it by marking the confirmation terminal — its
              // terminal guard then skips, leaving the (bare) poll render
              // as the sole renderer. Matches the send-time degraded gate.
              if ((addResult.newLinks || []).some(l => !l?.qurlId)) {
                db.markConfirmTerminal(sendId).catch((err) => {
                  logger.warn('markConfirmTerminal (add-recipients degrade) failed; fast-path could briefly stamp a partial counter until poll re-renders bare', { sendId, error: err.message });
                });
              } else {
                // Re-arm the fast-path with the post-add totals so a view
                // landing after /qurl add renders against the new base +
                // count + optional inline qurl_id fallback cache. PARTIAL
                // update — omitting
                // interactionToken/appId leaves them untouched
                // (saveSendConfirmState skips undefined keys) so it can't
                // null the live token. Best-effort + logged-swallowed.
                db.saveSendConfirmState(sendId, {
                  expectedCount: totalSent,
                  confirmBaseMsg: confirmMsg,
                  confirmQurlIds: allQurlIds,
                }).catch((err) => {
                  logger.warn('saveSendConfirmState (add-recipients re-persist) failed; fast-path renders pre-add totals until poll catches up', {
                    sendId, error: err.message,
                  });
                });
              }
            }

            await selectInteraction.editReply({ content: addResult.msg, components: [] });
          } catch (err) {
            const isTimeout = err?.code === 'InteractionCollectorError' || err?.message?.includes('time');
            // Generic user-facing message; real details only land in logs.
            const msg = isTimeout ? 'Selection timed out.' : 'Failed to add recipients. Please try again.';
            // Drop to warn for routine user-timeouts; keep error for real failures.
            const log = isTimeout ? logger.warn : logger.error;
            log('Add recipients failed', { sendId, error: err.message, isTimeout });
            await btnInteraction.editReply({ content: msg, components: [] }).catch(logIgnoredDiscordErr);
          }
        } finally {
          addRecipientsLocks.delete(sendId);
        }
      }
    });

    collector.on('end', (_, reason) => {
      // stop() freezes the final getFullMsg() render internally, so the
      // post-stop getFullMsg() below preserves the last-known counter
      // (vs. re-rendering against a cleared linkStatus → `0 of N viewed`).
      monitor.stop();
      if (reason === 'time') {
        // After a SUCCESSFUL revoke, re-render the revoke result
        // instead of the pre-revoke confirmMsg + Management-window
        // banner — otherwise the user's "Revoked X/Y links" view
        // gets clobbered when the collector window ends. Gate on
        // `revokeSucceeded` (not `revokeInFlight`) so a failure
        // message ("Failed to revoke links…") isn't overwritten with
        // a stale "Revoked 0/0 links" line.
        if (revokeSucceeded) {
          if (!revokeResultKnown) {
            interaction.editReply({
              content: 'Links for this send have already been revoked.',
              components: [],
            }).catch(logIgnoredDiscordErr);
            return;
          }
          // Terminal state: re-render content (Show Recipients may have
          // toggled), strip components. Omit `files`/`attachments`
          // so Discord keeps the existing revoked-users.txt without
          // re-uploading the same blob 15min later.
          const final = renderRevokeMsg(sendId, revokeResultUserNames, revokeResultTotal, revokeShowAll, revokeResultSuccess);
          interaction.editReply({ content: final.content, components: [] }).catch(logIgnoredDiscordErr);
          return;
        }
        // Revoke attempted but failed — leave the failure message.
        if (revokeInFlight) return;
        // Management window closed — this is the confirmation's terminal
        // render. Freeze it so a webhook fast-path edit arriving after the
        // collector ended can't re-animate the counter over the
        // window-closed banner. Fire-and-forget .catch in this sync 'end'
        // handler (best-effort, logged-swallowed); the monitor.stop()
        // above froze the poll, leaving the fast-path as the only
        // off-monitor editor to fence. Expired is intentionally NOT fenced
        // here — the qurl.expired handler edits the recipient DM, never
        // this sender confirmation, so it needs no terminal mark.
        db.markConfirmTerminal(sendId).catch((err) => {
          logger.warn('markConfirmTerminal (window-closed) failed; a late fast-path edit could briefly re-render the counter', { sendId, error: err.message });
        });
        interaction.editReply({
          content: monitor.getFullMsg() + '\n\n⏰ **Management window closed** — use `/qurl revoke` to revoke later.',
          components: [],
        }).catch(logIgnoredDiscordErr);
      }
    });
  }
}

// Handle adding new recipients to an existing send. senderDiscordId is
// derived from originalInteraction directly so no caller can pass a
// mismatched value and accidentally let one user add recipients to another
// user's send.
async function handleAddRecipients(sendId, usersCollection, originalInteraction, apiKey) {
  const senderDiscordId = originalInteraction.user.id;
  const sendConfig = await db.getSendConfig(sendId, senderDiscordId);
  if (!sendConfig) {
    return { msg: 'Send configuration not found.', newLinks: [], delivered: 0, failed: 0, newRecipients: [] };
  }

  // getSendConfig runs after the user-select await, so revoked_at catches
  // button, slash-command, and out-of-band revokes that landed while the
  // Add Recipients picker was open. A revoke after this point can still race
  // until recordQURLSendBatch grows a conditional write (#862).
  if (sendConfig.revoked_at) {
    return {
      msg: 'Cannot add recipients — this send has already been revoked.',
      newLinks: [], delivered: 0, failed: 0, newRecipients: [],
    };
  }

  // #352 entry gate. Shares the same `EXPIRY_LABELS` membership
  // predicate as the executeSendPipeline failGate
  // (`grep "expiresIn must be one of"`), closing the unprotected
  // path where a stale/regressed sendConfig row could ship an
  // off-set value. Protects BOTH downstream `expiryToMs` AND
  // `expiryToISO` (each used below for mint + DM dispatch) from
  // their shared silent-24h-default fallback. Failure shape
  // diverges from failGate intentionally: failGate throws (caller
  // catches at the slash-command boundary), while this gate returns
  // an error object — handleAddRecipients's caller renders the
  // string on the post-send confirm card, where a throw would land
  // as a generic "Internal error" with no actionable message.
  if (!isValidExpiry(sendConfig.expires_in)) {
    // warn (not error): a stale/corrupted DDB row is user-recoverable
    // (re-send), not paging-worthy. Matches renderConfirmCardRows's
    // analogous off-EXPIRY_LABELS log level. Forensics-only signal.
    logger.warn('addRecipients refused invalid expires_in', { sendId, expiresIn: truncForLog(sendConfig.expires_in) });
    return {
      msg: `Cannot add recipients — this send's saved expiry is invalid (the original send's links still work; create a new send to reach additional recipients).`,
      newLinks: [], delivered: 0, failed: 0, newRecipients: [],
    };
  }

  // Filter out bots and the sender. Convert the Discord Collection to a
  // plain array so later callers (map/forEach over newRecipients[i]) work.
  const newRecipients = [...usersCollection
    .filter(u => !u.bot && u.id !== senderDiscordId)
    .values()];
  // {id, username} returned on every path after this point so the
  // caller can extend its recipients[] (post-Add revoke shows
  // names). The success path is the only one where this is load-
  // bearing; failure paths return it for contract consistency, and
  // the caller's `successSet.has(r.id)` filter excludes phantom
  // IDs from any path that didn't write qurl_sends rows.
  const resolvedRecipients = newRecipients.map(u => ({ id: u.id, username: u.username }));

  if (newRecipients.length === 0) {
    return { msg: 'No valid recipients selected (bots and yourself are excluded).', newLinks: [], delivered: 0, failed: 0, newRecipients: resolvedRecipients };
  }

  // Create new QURL links for each resource type in the send config
  // recipientLinks[recipientId] = [{ qurlLink, resourceId, resType, label }]
  const recipientLinks = {};
  const hasFile = sendConfig.connector_resource_id;
  const hasLocation = sendConfig.actual_url;

  if (!hasFile && !hasLocation) {
    return { msg: 'Cannot add recipients — send configuration is incomplete.', newLinks: [], delivered: 0, failed: 0, newRecipients: resolvedRecipients };
  }

  // Tracks which prep paths actually completed so we can emit a single
  // upload_success per send (not one per kind). A sendConfig with both
  // file + location would otherwise fire two events for the same send,
  // which double-counts UploadCount in CloudWatch unless the metric
  // filter dimensions on `kind` (it doesn't, currently — see
  // qurl-integrations-infra#309). The collapsed event keeps UploadCount
  // = "number of fully-prepared sends" regardless of kind composition.
  const preparedKinds = [];
  // Inherit the original send's self-destruct timer so additional
  // recipients see the same vanish behavior. Persisted as a REAL/Number
  // column; both stores return null when unset. Hoisted above the file/
  // location branches because both pull the same value — the branches
  // can both fire for a sendConfig that had both kinds, and a per-branch
  // recompute would invite drift.
  const inheritedDestruct = sendConfig.self_destruct_seconds ?? null;
  // activeKind tracks which branch is in-flight when the outer catch
  // fires. The inner file try/catch returns on file failure, so by the
  // time we reach the outer catch the failure was NOT in the file
  // branch — `hasFile ? 'file' : 'location'` would mis-label mixed
  // sends. Per-branch assignment is the durable fix.
  let activeKind = null;
  try {
    if (hasFile) {
      activeKind = 'file';
      // Re-download from the stored Discord CDN URL, then upload a fresh
      // resource so the 10-token pool is full. Re-upload again every
      // TOKENS_PER_RESOURCE recipients. The original resource is drained by
      // the initial send, so we CANNOT reuse sendConfig.connector_resource_id.
      if (!sendConfig.attachment_url) {
        return {
          msg: 'Cannot add file recipients — original attachment is no longer available. Please create a new send.',
          newLinks: [], newRecipients: resolvedRecipients,
          delivered: 0,
          failed: 0,
        };
      }
      // Defense-in-depth: re-validate the stored URL is a real Discord CDN URL
      // before we re-download. If the DB row was ever tampered with (SQLi,
      // EFS access) this closes the SSRF that would otherwise point fetch
      // somewhere else.
      if (!isAllowedSourceUrl(sendConfig.attachment_url)) {
        logger.error('addRecipients refused non-Discord attachment_url', { sendId });
        return {
          msg: 'Cannot add file recipients — original attachment URL is no longer valid. Please create a new send.',
          newLinks: [], newRecipients: resolvedRecipients,
          delivered: 0,
          failed: 0,
        };
      }

      const expiresAt = expiryToISO(sendConfig.expires_in);
      let allLinks = [];
      let fileBuffer = null;
      const filename = sendConfig.attachment_name || 'file';
      const contentType = sendConfig.attachment_content_type || 'application/octet-stream';

      try {
        // Initial download+upload gives us the buffer for subsequent re-uploads.
        const first = await downloadAndUpload(sendConfig.attachment_url, filename, contentType, apiKey, inheritedDestruct);
        fileBuffer = first.fileBuffer;
        allLinks = await mintLinksInBatches({
          initialResourceId: first.resource_id,
          reuploadFn: () => reUploadBuffer(fileBuffer, filename, contentType, apiKey, inheritedDestruct),
          expiresAt,
          recipientCount: newRecipients.length,
          apiKey,
          selfDestructSeconds: inheritedDestruct,
          // Guild-scope the mint for watermark attribution (#1101). Add
          // Recipients reuses the original send's guild via
          // originalInteraction, so rows added here are attributable on
          // /qurl detect the same as the initial send's rows. Without this
          // the filter would silently drop added recipients (fails closed —
          // no leak — but a real watermark would read "no match").
          guildId: originalInteraction.guildId,
        });
      } catch (err) {
        // Discord CDN URLs are signed and expire (~24h). If the re-download
        // fails, surface a clear user-facing message; log the real error
        // server-side so err.message (which may echo upstream response detail)
        // never reaches a Discord reply. (The expiry-shaped user copy is
        // pre-existing; decoupling it from the network case is tracked in #634.)
        const isExpired = /403|expired|network|CDN/i.test(err.message || '');
        const msg = isExpired
          ? 'Original attachment URL has expired. Please create a new send.'
          : 'Failed to prepare links. Please try again, or create a new send if the issue persists.';
        logger.error('addRecipients file re-upload failed', {
          sendId, error: err.message, apiCode: err.apiCode, status: err.status, isExpired,
        });
        // Always emit — every failure here (CDN re-download, connector
        // re-upload, or mint) is a "couldn't create links" event. A rare,
        // expected CDN-expiry (Add Recipients on a >24h-old send) is absorbed
        // by the alarm's sustained threshold, exactly like pool-exhaustion — no
        // source-side skip. (An earlier message/phase-based skip here risked
        // silently suppressing real connector 403/auth outages.) quota_exceeded
        // — the one genuinely high-volume normal condition — is still skipped
        // inside emitMintFailureAudit.
        emitMintFailureAudit(err, { sendId, kind: 'file' });
        return { msg, newLinks: [], delivered: 0, failed: 0, newRecipients: resolvedRecipients };
      }

      if (allLinks.length < newRecipients.length) {
        // Underdelivery (fewer links than recipients) is NOT emitted as a
        // QURL_SEND_CREATE_LINK_FAILURE: it isn't a thrown error, the user
        // gets a clear "Only N of M" message, and it's a distinct shape from
        // the total-failure the event tracks. The adjacent connector
        // "200 + missing resource_id" shape is covered by its own alarm.
        logger.error('mintLinks returned fewer links than expected in addRecipients', { expected: newRecipients.length, got: allLinks.length });
        return { msg: `Only ${allLinks.length} of ${newRecipients.length} links created. Try again.`, newLinks: [], delivered: 0, failed: 0, newRecipients: resolvedRecipients };
      }
      // Iterate by allLinks length so an off-by-one can never index out of bounds.
      // The guard above ensures allLinks.length >= newRecipients.length.
      for (let i = 0; i < allLinks.length; i++) {
        const r = newRecipients[i];
        if (!r) break;
        if (!recipientLinks[r.id]) recipientLinks[r.id] = [];
        recipientLinks[r.id].push({
          qurlLink: allLinks[i].qurl_link,
          qurlId: allLinks[i].qurl_id,
          resourceId: allLinks[i].resourceId,
          resType: RESOURCE_TYPES.FILE,
          label: `File (${sanitizeFilename(sendConfig.attachment_name || 'file')})`,
        });
      }
      preparedKinds.push('file');
    }
    if (hasLocation) {
      activeKind = 'location';
      const locPayload = { type: 'google-map', url: sendConfig.actual_url, name: sendConfig.location_name || 'Google Maps Location' };
      const firstUpload = await uploadJsonToConnector(locPayload, 'location.json', apiKey, inheritedDestruct);
      const expiresAt = expiryToISO(sendConfig.expires_in);
      const allLinks = await mintLinksInBatches({
        initialResourceId: firstUpload.resource_id,
        reuploadFn: () => uploadJsonToConnector(locPayload, 'location.json', apiKey, inheritedDestruct),
        expiresAt,
        recipientCount: newRecipients.length,
        apiKey,
        selfDestructSeconds: inheritedDestruct,
        // Guild-scope for attribution (#1101) — see the file branch above.
        guildId: originalInteraction.guildId,
      });

      if (allLinks.length < newRecipients.length) {
        logger.error('mintLinks returned fewer links than expected in addRecipients (location)', { expected: newRecipients.length, got: allLinks.length });
        return { msg: `Only ${allLinks.length} of ${newRecipients.length} location links created. Try again.`, newLinks: [], delivered: 0, failed: 0, newRecipients: resolvedRecipients };
      }
      newRecipients.forEach((r, i) => {
        if (!recipientLinks[r.id]) recipientLinks[r.id] = [];
        recipientLinks[r.id].push({
          qurlLink: allLinks[i].qurl_link,
          qurlId: allLinks[i].qurl_id,
          resourceId: allLinks[i].resourceId,
          resType: RESOURCE_TYPES.MAPS, label: sendConfig.location_name || 'Google Maps',
        });
      });
      preparedKinds.push('location');
    }
  } catch (error) {
    logger.error('Failed to create links for additional recipients', {
      sendId, error: error.message, apiCode: error.apiCode, status: error.status,
    });
    const isPoolExhausted = error.message?.includes('429') || error.message?.includes('limit');
    const msg = isPoolExhausted
      ? 'Link pool exhausted for this resource. Please create a new send instead of adding recipients.'
      : 'Failed to create links for new recipients.';
    // kind: activeKind — set on entry to each branch (see its decl above);
    // a future refactor that throws before either branch lands kind=null,
    // discoverable in CloudWatch. quota_exceeded skip lives in emitMintFailureAudit.
    emitMintFailureAudit(error, { sendId, kind: activeKind });
    return { msg, newLinks: [], delivered: 0, failed: 0, newRecipients: resolvedRecipients };
  }

  // Single emission per send. `kind` carries the composition so a future
  // CloudWatch dimension on it can break the count down per kind without
  // double-counting mixed sends. Values: 'file' | 'location' | 'mixed'.
  if (preparedKinds.length > 0) {
    const kind = preparedKinds.length === 1 ? preparedKinds[0] : 'mixed';
    logger.audit(AUDIT_EVENTS.UPLOAD_SUCCESS, { send_id: sendId, kind });
  }

  const recipientIds = Object.keys(recipientLinks);
  if (recipientIds.length === 0) {
    return { msg: 'Failed to create any links.', newLinks: [], delivered: 0, failed: 0, newRecipients: resolvedRecipients };
  }

  // Hoist payload-shared inputs to the top of the dispatch block so
  // the entire batch shares one expiry timestamp and one resolved
  // sender alias. Mirrors executeSendPipeline's structure — both
  // pipelines now compute the Unix-seconds expiresAt once per call,
  // eliminating Date.now() drift between recipients in the same
  // batch. (The ISO-string `expiresAt` inside the file/location prep
  // branches calls expiryToISO independently for mintLinks — that's
  // a separate type for a separate consumer; sub-second drift between
  // the minted-link expiry and the DM-payload expiry is negligible
  // at the 30m–7d horizon.) resolveSenderAlias is a pure function of
  // originalInteraction so hoisting also avoids re-resolving the
  // nickname/globalName/username chain per dispatch.
  //
  // Computed BEFORE recordQURLSendBatch so a malformed
  // `sendConfig.expires_in` throwing in `expiryToMs` aborts the
  // dispatch BEFORE any DDB rows are written — otherwise the DB
  // writes would happen, then the throw would bubble out with no DMs
  // sent, leaving orphan QURL records with no recipient. The
  // `handleAddRecipients` path is especially exposed here because
  // sendConfig.expires_in is read from DDB (rather than validated
  // upstream like executeSendPipeline's slash-command param), so a
  // stale/regressed row predating the current validation would slip
  // through. Throwing pre-write makes the failure mode visible at
  // the function level rather than as orphan rows. Closes #352.
  const expiresAt = Math.floor((Date.now() + expiryToMs(sendConfig.expires_in)) / 1000);
  const senderAlias = resolveSenderAlias(originalInteraction);
  const { guildName, guildIconUrl } = resolveGuildProvenance(originalInteraction);

  // Persist to DB before DMs
  const batchSends = [];
  for (const [rid, links] of Object.entries(recipientLinks)) {
    for (const link of links) {
      batchSends.push({
        sendId, senderDiscordId, recipientDiscordId: rid, resourceId: link.resourceId,
        resourceType: link.resType, qurlLink: link.qurlLink,
        // qurl_id threads through addRecipients the same way it does
        // through executeSendPipeline — needed for the qurl.expired
        // webhook handler's GSI lookup. See recordQURLSendBatch.
        qurlId: link.qurlId,
        expiresIn: sendConfig.expires_in,
        channelId: originalInteraction.channelId, targetType: 'user',
        // guild_id for /qurl detect attribution (#1101) — mirrors the mint
        // above + executeSendPipeline's row write. Reuses the original
        // send's guild so added recipients stay attributable. Sparse write.
        guildId: originalInteraction.guildId,
      });
    }
  }
  // Same guarantee as executeSendPipeline: if the DB write fails, abort
  // BEFORE any DMs go out so we don't leave live QURL links with no
  // local record.
  try {
    await db.recordQURLSendBatch(batchSends);
  } catch (err) {
    logger.error('recordQURLSendBatch failed in addRecipients; aborting before DMs', {
      sendId, error: err.message, linkCount: batchSends.length,
    });
    return {
      msg: 'Failed to save link records. Recipients were not messaged. Please try again.',
      newLinks: [], newRecipients: resolvedRecipients,
      delivered: 0,
      failed: 0,
    };
  }

  // Send DMs — one message per recipient with all their links
  let delivered = 0;
  let failed = 0;

  const dmResults = await batchSettled(newRecipients, async (recipient) => {
    const links = recipientLinks[recipient.id];
    // Defensive guard: recipientLinks is populated above for every newRecipient,
    // so reaching this branch means an upstream invariant broke (recipient
    // present in the loop input but missing from the link map). We skip
    // both the network call AND the audit emission — the metric represents
    // actual dispatch attempts, not "recipients listed in the input." A
    // dropped audit here is correct: there is no transport leg to count.
    if (!links || links.length === 0) return { sent: false, username: recipient.username };

    // links.slice(0, 10) caps at Discord's 10-embed-per-message limit.
    // The button-row chunking below splits all buttons into ActionRows of
    // 5 (Discord's per-row cap). Note Discord renders all embeds first,
    // then all component rows below — buttons are NOT visually paired
    // with their corresponding embed. Multi-link UX would benefit from
    // per-link button labels (e.g. "Step Through · report.pdf"), but
    // today links.length is always 1 so labels are uniform.
    // try/finally + before-DB-write — see executeSendPipeline's
    // batchSettled callback for the full rationale (payload-build,
    // sendDM-throws, AND DB-throw must all still emit the metric).
    // Wraps the entire dispatch attempt — payload assembly, button re-
    // packing, network call — so a malformed sendConfig (e.g. pathological
    // personalMessage that throws inside buildDeliveryPayload) still
    // counts as dispatch_failed instead of disappearing from CloudWatch.
    let result = { ok: false };
    try {
      // links.slice(0, 10) caps at Discord's 10-embed-per-message
      // limit. The embed body is identical per-link (sender + guild +
      // expiry don't vary), so build the EmbedBuilder once and repeat
      // the reference N times. discord.js serializes each embeds[]
      // entry via .toJSON() — a pure read of internal state — so
      // reference sharing is safe. Saves N-1 EmbedBuilder allocations
      // + N-1 sanitize-chain runs on the senderAlias/guildName halves.
      // packBulkDeliveryComponents enforces 1 <= len <= 10 with
      // fail-loud throws; the upstream guard at line 2372 above
      // (`if (!links || links.length === 0)`) is what keeps us out
      // of the lower-bound throw. If a future refactor splits this
      // dispatch loop or drops that guard, the throw will surface
      // here at the helper boundary — see packBulkDeliveryComponents
      // docstring for the contract.
      const cappedLinks = links.slice(0, 10);
      const sharedEmbed = buildDeliveryEmbed({
        senderAlias,
        guildName,
        guildIconUrl,
        expiresAt,
        personalMessage: sendConfig.personal_message,
      });
      const allEmbeds = Array(cappedLinks.length).fill(sharedEmbed);
      const allComponents = packBulkDeliveryComponents(cappedLinks.map(link => link.qurlLink));

      result = await sendDM(recipient.id, { embeds: allEmbeds, components: allComponents });
    } finally {
      logger.audit(result.ok === true ? AUDIT_EVENTS.DISPATCH_SENT : AUDIT_EVENTS.DISPATCH_FAILED, { send_id: sendId });
    }
    // One persist write per recipient regardless of links.length —
    // both store methods key on (send_id, recipient_discord_id).
    await persistDispatchResult(sendId, recipient.id, result);
    return { sent: result.ok === true, username: recipient.username };
  }, 5);

  for (const r of dmResults) {
    if (r.status === 'fulfilled' && r.value.sent) delivered++;
    else failed++;
  }

  // newLinks pairs each qurlId with the recipient's username so the
  // monitor can seed linkStatus correctly. Walking recipientLinks
  // (rather than the flattened list) preserves the per-recipient
  // association — a /qurl add user opening their link must surface
  // under their username, not a positional guess.
  const newLinks = [];
  for (const [rid, links] of Object.entries(recipientLinks)) {
    const recip = resolvedRecipients.find(r => r.id === rid);
    const username = recip?.username || 'unknown';
    for (const link of links) {
      if (link.qurlId) newLinks.push({ qurlId: link.qurlId, username });
    }
  }
  let msg = `Added ${delivered} recipient${delivered !== 1 ? 's' : ''}`;
  if (failed > 0) msg += ` (${failed} could not be reached)`;
  logger.info('/qurl add recipients', { sendId, delivered, failed });
  return { msg, newLinks, delivered, failed, newRecipients: resolvedRecipients };
}

// --- /qurl revoke handler ---

// Discord caps StringSelectMenuOption label at 100 chars and description
// at 100 chars. Truncating to 99 + `…` leaves one for safety and keeps the
// rendering predictable across Discord clients.
// https://discord.com/developers/docs/interactions/message-components#select-menu-object-select-option-structure
const SELECT_MENU_FIELD_MAX = 100;

function truncate(s, max = SELECT_MENU_FIELD_MAX) {
  if (!s) return '';
  return s.length <= max ? s : `${s.slice(0, max - 1)}…`;
}

// Render the /qurl revoke dropdown label from a row returned by
// db.getRecentSends. Prefers the human-identifiable bit (filename or
// location name) over the previous abstract `${resource_type} to N users`.
// Falls back gracefully for legacy rows that predate qurl_send_configs
// being populated (LEFT JOIN produces null fields there).
//
// `channelName` is passed in by the caller (handleRevoke) from
// `interaction.guild?.channels.cache.get(s.channel_id)?.name`. Discord
// mention syntax (`<#id>`) does NOT render inside StringSelectMenu
// option labels — passing the name explicitly is the only way to avoid
// showing a raw snowflake to the user.
function formatRevokeLabel(s, channelName) {
  const recipients = s.recipient_count === 1 ? '1 person' : `${s.recipient_count} people`;
  const channelRef = s.target_type === 'user'
    ? 'DM'
    : (channelName ? `#${channelName}` : `${s.target_type} channel`);

  if (s.resource_type === 'file') {
    const name = s.attachment_name || 'file';
    return truncate(`📎 ${name} — ${recipients} (${channelRef})`);
  }
  if (s.resource_type === 'location') {
    const name = s.location_name || 'location';
    return truncate(`📍 ${name} — ${recipients} (${channelRef})`);
  }
  // Unknown resource type — keep the old abstract shape so we never
  // render a bare empty label.
  return truncate(`${s.resource_type} — ${recipients} (${channelRef})`);
}

function formatRevokeDescription(s) {
  const when = new Date(s.created_at).toLocaleString();
  const delivery = `${s.delivered_count}/${s.recipient_count} delivered`;
  const expiry = `expires ${s.expires_in}`;
  const base = `${when} · ${delivery} · ${expiry}`;
  // If there's space left, append a truncated message preview so users
  // can disambiguate sends with the same filename but different notes.
  if (s.personal_message) {
    const remaining = SELECT_MENU_FIELD_MAX - base.length - 4; // ` · "…"` overhead
    if (remaining > 8) {
      return truncate(`${base} · "${truncate(s.personal_message, remaining)}"`);
    }
  }
  return truncate(base);
}

// Single non-terminal stage between createFlow and deleteFlow.
// Stage name is the source of truth for the dispatcher routing
// table (registerFlow at the bottom of the file) — changing it is
// a single edit.
const REVOKE_STAGE_AWAITING_SELECT = 'awaiting_revoke_select';

// customId for the select menu. PREFIX-ONLY — no nonce, no flow_id
// encoded. Concurrent revokes deliberately drop: the second call
// supersedes the first via deleteFlow + createFlow. See
// flow-dispatch's trust-model comment on why customId is unsafe as
// an identity signal.
const REVOKE_SELECT_CUSTOM_ID = 'qurl_revoke_select';

// TTL for the awaiting-select flow row, in seconds. Matches the
// pre-existing 60-second click window for the menu. DDB TTL reap is
// asynchronous (~48 h) but loadFlow filters expired rows
// synchronously, so the UX cuts off at 60 s on the dot.
const REVOKE_FLOW_TTL_SECONDS = 60;

async function handleRevoke(interaction, apiKey) {
  if (!apiKey) {
    return interaction.reply({ content: 'qURL API key is not configured.', ephemeral: true });
  }
  await interaction.deferReply({ ephemeral: true });

  const recentSends = await db.getRecentSends(interaction.user.id, 5);

  if (recentSends.length === 0) {
    return interaction.editReply({ content: 'No recent sends to revoke.' });
  }

  // Open the flow row BEFORE rendering the menu. If the harness
  // fails (DDB outage, IAM regression), we want the user to see an
  // error immediately rather than a clickable menu whose selection
  // would then 404 against the dispatcher.
  //
  // Supersede semantics: a user with an existing in-flight revoke
  // flow who runs `/qurl revoke` again gets the old flow torn down
  // and a fresh menu. The orphan menu in the old message becomes a
  // no-op (its selection event will hit the dispatcher, miss on
  // loadFlow, and surface the "superseded — run again" message).
  // This is a deliberate exception to the design-doc rule that
  // "a user with an existing non-expired flow in a given channel
  // cannot start a second flow there" — for revoke specifically,
  // the menu is a stateless listing and the second invocation is
  // idempotent, so blocking it would just confuse users who can't
  // remember whether they cancelled the prior menu.
  //
  // supersedeOrCreate (flow-state.js) encapsulates the create →
  // load → version-gated delete → retry sequence shared by all
  // two-stage flows; the only flow-specific shape here is the
  // sibling-flow disambiguation when a non-revoke row owns the
  // flow_id.
  const flow_id = flowIdForInteraction(interaction);
  let supersede;
  try {
    supersede = await supersedeOrCreate({
      flow_id,
      stage: REVOKE_STAGE_AWAITING_SELECT,
      payload: null,
      ttl_seconds: REVOKE_FLOW_TTL_SECONDS,
    });
  } catch (err) {
    // DDB throttle / IAM blip — surface as recoverable rather than
    // letting it propagate to the generic "error executing command"
    // reply. supersedeOrCreate internally already retried once, so
    // throws here are real (not transient OCC churn).
    logger.warn('handleRevoke: supersedeOrCreate threw', {
      flow_id, error: err && err.message,
    });
    return interaction.editReply({
      content: 'Could not start a revoke session — please try again.',
    });
  }
  if (!supersede.created) {
    // A sibling flow owns this flow_id (or we lost two OCC races
    // in a row). Look up the sibling-message via the dispatcher's
    // registry — known sibling stage (setup-modal etc.) gets
    // actionable wording naming the command the user should
    // finish or cancel first; an unknown or vanished surviving
    // row falls through to the generic "try again."
    logger.warn('handleRevoke: supersedeOrCreate did not claim slot', {
      flow_id, surviving_stage: supersede.surviving?.stage ?? null,
    });
    const siblingMsg = siblingMessageForStage(supersede.surviving?.stage);
    if (siblingMsg) {
      return interaction.editReply({ content: siblingMsg });
    }
    return interaction.editReply({
      content: 'Could not start a revoke session — please try again.',
    });
  }

  const select = new StringSelectMenuBuilder()
    .setCustomId(REVOKE_SELECT_CUSTOM_ID)
    .setPlaceholder('Select a send to revoke')
    .addOptions(recentSends.map(s => {
      // StringSelectMenu labels are plain text — <#id> mention syntax
      // does NOT resolve. Look up the channel name from the guild cache
      // so users see `#general` instead of `#1123456789012345678`.
      // Cache hit is cheap; if cache is cold (DM context, bot just
      // restarted) the formatter falls through to a generic label.
      const channelName = s.channel_id
        ? interaction.guild?.channels.cache.get(s.channel_id)?.name ?? null
        : null;
      return {
        label: formatRevokeLabel(s, channelName),
        description: formatRevokeDescription(s),
        value: s.send_id,
      };
    }));

  const row = new ActionRowBuilder().addComponents(select);
  await interaction.editReply({
    content: 'Select a send to revoke all its links:',
    components: [row],
  });
}

// `reason: 'terminal'` reflects the FLOW lifecycle, not the qURL
// API call's success. The state machine terminated when the user
// picked an item; downstream revoke outcome is captured by its
// own `revoke_success` / `revoke_failed` audit events.
//
// `deleteFlow` is the at-most-once dedup primitive — only the
// worker whose conditional delete returns `{ deleted: true }`
// proceeds. A duplicate event (SQS at-least-once redelivery in
// the future worker tier, or a Discord double-dispatch today) sees
// `{ deleted: false }` and exits.
async function handleRevokeSelect(interaction, { flow_id }) {
  // Parallelize the dedup primitive (deleteFlow) with the apiKey
  // resolution. Safe here because getGuildApiKey is an idempotent
  // DDB read with zero side effects — the dedup loser short-circuits
  // before the qURL API call, so a duplicate event never multiplies
  // the parallel call's cost.
  //
  // Rule for future handlers in PR 6/7/8/9: parallelize the dedup
  // primitive ONLY with idempotent / free reads. Do NOT parallelize
  // with a paid side-effect call (a Google Places lookup, a recipient
  // resolution that hits an external API, a connector upload) —
  // at-least-once redelivery would then bill or trigger the side
  // effect once per duplicate, defeating the dedup gate's purpose.
  const [guildApiKey, deleteResult] = await Promise.all([
    interaction.guildId ? db.getGuildApiKey(interaction.guildId) : null,
    deleteFlow(flow_id, {
      stage: REVOKE_STAGE_AWAITING_SELECT,
      reason: 'terminal',
    }),
  ]);
  const apiKey = guildApiKey || config.QURL_API_KEY;

  if (!deleteResult.deleted) {
    // Another worker already completed (or admin_cleanup'd) this flow.
    // Reply rather than update — `interaction.update` would mutate
    // the original ephemeral message, but a duplicate event by
    // definition means the original was already updated by the
    // winning worker. Ephemeral reply preserves the user-visible
    // first result.
    return interaction.reply({
      content: 'This revoke was already processed.',
      ephemeral: true,
    }).catch(logIgnoredDiscordErr);
  }

  if (!apiKey) {
    return interaction.update({
      content: 'qURL API key is no longer configured for this server.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }

  const sendId = interaction.values[0];
  const revoked = await revokeAllLinks(sendId, interaction.user.id, apiKey, resolveSenderAlias(interaction));

  // Slash-command path lacks the in-scope `recipients` array needed
  // to resolve names → no "Revoked for: …" line here. Operators
  // wanting names should use the inline button after a send.
  await interaction.update({
    content: buildRevokeHeader(revoked.success, revoked.total),
    components: [],
  });
}

// /qurl setup — legacy modal-paste path conversion. See
// docs/zero-downtime-design.md Pillar 1 for the two-stage shape:
//
//   /qurl setup (slash)          createFlow(awaiting_setup_button)
//       → reply with [Configure qURL] button
//   button click (dispatcher)    transitionFlow → awaiting_setup_modal
//       → showModal()                              (set_expires_at extends TTL)
//   modal submit (dispatcher)    validate + persist + deleteFlow(terminal)
//       → editReply(success)
//
// The OAuth path (when AUTH0_* is configured) is NOT flow-state-
// backed — its persistence is the signed state token + the
// pending_links DDB table, and there's no `await*` in process to
// convert.
const SETUP_STAGE_AWAITING_BUTTON = 'awaiting_setup_button';
const SETUP_STAGE_AWAITING_MODAL = 'awaiting_setup_modal';
const SETUP_BUTTON_CUSTOM_ID = 'qurl_setup_button';
const SETUP_MODAL_CUSTOM_ID = 'qurl_setup_modal';
const SETUP_MODAL_FIELD_API_KEY = 'api_key';

// Two-stage TTL budget. The button-stage window covers "click the
// button" — short enough that an abandoned button is naturally
// superseded, long enough that a mobile admin who app-switches to
// a password manager doesn't come back to an expired button. The
// modal-stage window is the real budget for finding and pasting
// the key.
const SETUP_BUTTON_TTL_SECONDS = 120;
const SETUP_MODAL_TTL_SECONDS = 300;

// TODO(upstream-rebrand): regex + setMaxLength below mirror the
// upstream qurl-service key-format spec. A future rebrand or
// JWT-shaped key format must update both in lockstep — grep with
// `git grep TODO(upstream-rebrand)` lands every mirroring site.
const SETUP_API_KEY_REGEX = /^lv_(live|test)_[A-Za-z0-9_-]{20,}$/;
// Length floor + ceiling for the modal's TextInput, paired with
// SETUP_API_KEY_REGEX above. The floor (28) derives from
// `lv_live_`-prefix (8 chars) + the regex suffix floor of 20 chars;
// the ceiling (64) is a defense-in-depth cap for pathological
// pastes (Discord's default is 4000). Every member of the trio
// must move in lockstep — a future JWT-shaped key format that
// exceeds the ceiling would silently truncate at the modal and
// surface as a misleading "Invalid API key format" reply. Keeping
// the constants adjacent makes the lockstep impossible to update
// independently.
const SETUP_API_KEY_MIN_LENGTH = 28;
const SETUP_API_KEY_MAX_LENGTH = 64;

// User-visible success message on successful setup. Exported via
// _test so test assertions read the production string instead of
// hardcoding a copy that drifts.
const SETUP_SUCCESS_MSG =
  '✅ **qURL is now configured for this server!**\n\n'
  + (config.MAP_COMMAND_ENABLED
    ? 'Your team can use `/qurl send` and `/qurl map` to share files and locations securely.\n'
    : 'Your team can use `/qurl send` to share files securely.\n')
  + 'All qURL usage will be billed to your API key.';

// Button-stage handler. Routed by flow-dispatch when the admin
// clicks the [Configure qURL] button rendered by handleSetup.
//
// Sequence: transitionFlow first (it's the OCC primitive that
// rejects concurrent button clicks), then showModal as the ACK on
// the success branch. `showModal` MUST be the interaction's first
// response — we cannot deferReply ahead of it — so the DDB round-
// trip happens inside Discord's 3 s ACK window. UpdateItem latency
// is typically 50–200 ms; if a DDB outage pushes it close to the
// wall, the user sees Discord's "interaction failed" notice and
// can re-click. Acceptable tradeoff vs. losing OCC dedup.
//
// `set_expires_at` extends the row's TTL from the 120s button window
// (SETUP_BUTTON_TTL_SECONDS) to the 300s modal window
// (SETUP_MODAL_TTL_SECONDS), giving the admin a fresh budget from
// the moment the modal opens. This trips the `extended: true` audit
// flag on the FLOW_TRANSITION event — correct: the deadline really
// was extended.
async function handleSetupButton(interaction, { flow_id, row }) {
  const result = await transitionFlow(flow_id, row.version, {
    stage_to: SETUP_STAGE_AWAITING_MODAL,
    terminal: false,
    set_expires_at: Math.floor(Date.now() / 1000) + SETUP_MODAL_TTL_SECONDS,
  });

  // Early-return branches below use `interaction.reply` directly
  // (not safeReply): we haven't deferReply'd, so replied/deferred
  // are false on entry. Matches handleRevokeSelect's shape.
  if (result.result === 'conflict') {
    // Two concurrent button clicks. The OCC loser must NOT call
    // showModal — the winner will (or already did), and a duplicate
    // showModal on the same parent interaction surfaces as a
    // confusing Discord error.
    return interaction.reply({
      content: 'Another setup attempt is in progress — finish or close it, then retry.',
      ephemeral: true,
    }).catch(logIgnoredDiscordErr);
  }

  if (result.result === 'not_found') {
    // Flow row vanished between the dispatcher's loadFlow and the
    // transitionFlow Update (TTL reap or a concurrent
    // admin_cleanup). Tell the user to rerun.
    return interaction.reply({
      content: 'This setup session expired or was replaced — please run `/qurl setup` again.',
      ephemeral: true,
    }).catch(logIgnoredDiscordErr);
  }

  // Success — show the modal. It becomes the interaction's ACK.
  // (Non-CCFE transitionFlow failures throw and propagate to the
  // dispatcher's safety net; see flow-state.js for the contract.)
  const modal = new ModalBuilder()
    .setCustomId(SETUP_MODAL_CUSTOM_ID)
    .setTitle('Configure qURL');
  const keyInput = new TextInputBuilder()
    .setCustomId(SETUP_MODAL_FIELD_API_KEY)
    .setLabel('qURL API Key')
    .setPlaceholder('lv_live_your_key_here')
    .setStyle(TextInputStyle.Short)
    .setRequired(true)
    // TODO(upstream-rebrand): min+max mirror upstream qurl-service's
    // key-format bounds. Both constants live next to SETUP_API_KEY_REGEX
    // so the lockstep trio is impossible to update independently.
    .setMinLength(SETUP_API_KEY_MIN_LENGTH)
    .setMaxLength(SETUP_API_KEY_MAX_LENGTH);
  modal.addComponents(new ActionRowBuilder().addComponents(keyInput));

  // Roll back the OCC transition if showModal throws. Without the
  // delete, the row sits at awaiting_setup_modal until TTL and the
  // admin's /qurl setup rerun trips the supersede peek into the
  // "you already have a modal open" branch. If the rollback itself
  // also fails (DDB region outage), the error logs at error-level
  // — admin recovery is the SETUP_MODAL_TTL_SECONDS (5 min) wait.
  //
  // expectedVersion gates the rollback delete on the exact version
  // we just wrote. If a concurrent /qurl setup rerun's
  // supersedeOrCreate ALSO observed this row and won the version
  // race first, this delete fails (deleted:false) — correct: the
  // row that exists at flow_id is now the new caller's, not ours
  // to clean up. Without the version gate, two simultaneous
  // rollbacks across two clients could each "succeed" against
  // each other's rows.
  //
  // No spurious error log if a concurrent supersede ALSO cleared
  // this row first — CCFE in deleteFlow returns `{deleted: false}`
  // rather than throwing (flow-state.js), so the .catch below never
  // fires on that path.
  try {
    await interaction.showModal(modal);
  } catch (err) {
    // warn-level: the dominant cause is Discord token expiry /
    // transient REST blips that are fully recoverable by user
    // rerun. error-level is reserved for the rollback-also-failed
    // branch below, which is the genuine "this admin is stuck for
    // up to SETUP_MODAL_TTL_SECONDS" condition that warrants paging.
    logger.warn('handleSetupButton: showModal failed after transitionFlow committed', {
      flow_id, error: err && err.message,
    });
    await deleteFlow(flow_id, {
      stage: SETUP_STAGE_AWAITING_MODAL,
      reason: 'abort',
      // The version that landed after our successful transitionFlow
      // — anything else means a concurrent supersede already
      // advanced the row past us, in which case we don't own the
      // rollback anymore.
      expectedVersion: result.version,
    }).catch((rollbackErr) => {
      // `metric: 'setup_rollback_failed'` is the stable selector
      // for the CloudWatch alarm filter. Keying on the message
      // string instead would break the moment a copy tweak lands.
      logger.error('handleSetupButton: rollback deleteFlow failed', {
        flow_id,
        error: rollbackErr && rollbackErr.message,
        metric: 'setup_rollback_failed',
      });
    });
    // The "run /qurl setup again" guidance is accurate when the
    // rollback succeeded (clean state, rerun works). If the rollback
    // ALSO failed (rare double-DDB-failure logged just above), the
    // rerun will hit the "you already have a modal open" branch
    // until SETUP_MODAL_TTL_SECONDS elapses — the admin's recovery
    // is just to wait. Distinguishing the two cases in the message
    // would require reading the rollback outcome into a variable
    // (the .catch swallows it), and the wait-vs-retry distinction
    // is marginal UX in an already-rare error path; keep the
    // simpler wording.
    //
    // Use safeReply (followUp vs reply based on interaction state)
    // rather than a bare .reply.catch — showModal may have partially
    // acked before throwing, leaving interaction.replied=true; a
    // bare .reply would then throw InteractionAlreadyReplied and
    // silently swallow, leaving the admin with no feedback.
    await safeReply(
      interaction,
      'Could not open the configuration form — please run `/qurl setup` again.',
    );
  }
}

// Modal-submit handler. Routed by flow-dispatch when the admin
// submits the modal opened by handleSetupButton. The flow row's
// stage is already validated (`awaiting_setup_modal`) by the
// dispatcher.
//
// `deleteFlow` runs BEFORE the qURL API validation so the dedup
// gate fires on the OCC primitive — a duplicate modal submit
// (Discord retry, double-click) sees `deleted: false` and exits
// without burning a qURL API key validation call. Same ordering
// rationale as handleRevokeSelect.
async function handleSetupModal(interaction, { flow_id }) {
  // Modal-stage flow_state delete is terminal — the user committed
  // the form, the flow has lifecycled out.
  const { deleted } = await deleteFlow(flow_id, {
    stage: SETUP_STAGE_AWAITING_MODAL,
    reason: 'terminal',
  });
  if (!deleted) {
    // `deleted: false` collapses three real causes: TTL'd between
    // modal open and submit, concurrent admin_cleanup, and a duplicate
    // submit from Discord retry. The TTL case is the most plausible
    // (admin walks away mid-paste past the 300 s budget), so the
    // wording covers both "expired" and "already processed."
    return interaction.reply({
      content: 'This setup session has expired or was already processed — run `/qurl setup` again.',
      ephemeral: true,
    }).catch(logIgnoredDiscordErr);
  }

  // Structured-log fields shared across every validation failure
  // branch. A guild that hits "Invalid API key" 50 times in a row
  // (admin keeps re-pasting the wrong key, or a credential rotation
  // broke them) needs to surface in ops dashboards — that's only
  // possible if every failure logs guild_id + configured_by.
  // Bare reply paths (no .catch on the malformed-key `interaction.reply`
  // and the validation-failure `interaction.editReply` branches below)
  // are intentional: the flow row is already deleted by this point,
  // so a Discord throw propagates to the dispatcher's safety net
  // which replies "run again" — which IS the correct UX for these
  // branches (admin really should rerun). Contrast the success-
  // path .catch a few lines down, where "run again" would mislead
  // because the key was already persisted.
  //
  // Exception: the `!deleted` early-return above DOES .catch — the
  // dedup-loser path means another worker already replied to the
  // user, and a second "run again" from the safety net would
  // visually duplicate. The bare-reply rule applies only to the
  // post-deleteFlow validation branches.
  const logFields = {
    guild_id: interaction.guildId,
    configured_by: interaction.user.id,
  };

  // `String(...)` coerces any non-string return shape (a hypothetical
  // Discord SDK contract change) into a string before .trim(). A
  // null/undefined coerces to 'null'/'undefined', which the regex
  // format check below cleanly rejects as malformed — the defense's
  // job is just to keep .trim() from throwing, not to validate.
  // The flow row is already deleted by this point, so an uncaught
  // throw here would surface as "Something went wrong" AFTER the
  // row is gone — asymmetrically expensive vs. this one-token wrap.
  const submittedKey = String(
    interaction.fields.getTextInputValue(SETUP_MODAL_FIELD_API_KEY),
  ).trim();
  // Probable client-side truncation: the key landed exactly at the
  // setMaxLength ceiling. The regex check below will likely reject
  // (current `lv_*` keys are well under 64 chars), but if it ever
  // accepts a 64-char value the warn surfaces the truncation
  // candidate to ops — an upstream key-format change that exceeds
  // the cap would otherwise look like a guild-side "Invalid API
  // key format" loop with no trail back to this constant.
  if (submittedKey.length === SETUP_API_KEY_MAX_LENGTH) {
    logger.warn('validate-key probable truncation (key landed at SETUP_API_KEY_MAX_LENGTH)', {
      ...logFields,
      key_length: submittedKey.length,
    });
  }
  if (!SETUP_API_KEY_REGEX.test(submittedKey)) {
    logger.warn('validate-key rejected (bad format)', logFields);
    return interaction.reply({
      content: 'Invalid API key format. Keys start with `lv_live_` or `lv_test_` and are at least 28 characters. Run `/qurl setup` again to retry.',
      ephemeral: true,
    });
  }

  await interaction.deferReply({ ephemeral: true });
  try {
    const resp = await fetch(`${config.QURL_ENDPOINT}/v1/qurls?limit=1`, {
      headers: { 'Authorization': `Bearer ${submittedKey}`, 'Accept': 'application/json' },
      signal: AbortSignal.timeout(10000),
    });
    if (resp.status === 401 || resp.status === 403) {
      logger.warn('validate-key rejected by qURL API', { ...logFields, status: resp.status });
      return interaction.editReply({ content: '❌ **Invalid API key.** Double-check your key at **https://layerv.ai**.' });
    }
    if (!resp.ok) {
      logger.warn('validate-key non-2xx from qURL API', { ...logFields, status: resp.status });
      return interaction.editReply({ content: `❌ **qURL API error** (${resp.status}). Try again later.` });
    }
  } catch (err) {
    // Don't reflect err.message to Discord — network errors can
    // contain internal hostnames/IPs (e.g. "connect ECONNREFUSED
    // 10.0.0.5:8080") that should not leak to a guild admin's
    // screen. Same redaction shape as the pre-conversion code.
    logger.error('validate-key request failed', { ...logFields, error: err.message });
    return interaction.editReply({
      content: '❌ **Could not validate key.** Please try again in a moment.',
    });
  }

  await db.setGuildApiKey(interaction.guildId, submittedKey, interaction.user.id);
  logger.info('Guild API key configured', logFields);

  fireAndForgetLinkGuildWebhookSubscription({
    guildId: interaction.guildId,
    apiKey: submittedKey,
    via: 'paste',
    configuredBy: interaction.user.id,
  });

  // Swallow Discord errors on the post-persist editReply. If the
  // editReply throws AFTER setGuildApiKey commits, letting it
  // propagate would fire the dispatcher's universal safety-net
  // reply ("Something went wrong — please run the command again"),
  // which is actively wrong: the key IS saved. Admin can confirm
  // with /qurl status; the saved-but-no-confirmation case is rare
  // and recoverable, the misleading-error case is not.
  return interaction.editReply({
    content: SETUP_SUCCESS_MSG,
  }).catch(logIgnoredDiscordErr);
}

// ─────────────────────────────────────────────────────────────
// /qurl send + /qurl map — slash-driven sends.
//
// All-options-up-front slash commands that render an in-channel
// ephemeral confirm card. The flow:
//
//   /qurl send recipients:<text> attachment:<file> [expires-in]
//                                                  [self-destruct]
//                                                  [personal-message]
//   /qurl map  recipients:<text> location:<text>   [location-name]
//                                                  [expires-in]
//                                                  [self-destruct]
//                                                  [personal-message]
//
// After parsing/validating the slash options, the bot renders an
// in-channel ephemeral confirm card with the resolved recipient list
// (or a UserSelectMenu when `recipients:` was omitted) plus Send /
// Cancel buttons. The confirm card is flow_state-backed so the Send
// dedup gate fires on `deleteFlow` (consistent with /qurl revoke,
// /qurl setup).
// ─────────────────────────────────────────────────────────────

const {
  parseRecipientMentions,
  isVoiceChannelType,
  isBotMember,
  MAX_SLASH_OPTION_LENGTH: RECIPIENTS_SLASH_MAX_LENGTH,
} = require('./recipient-parser');

const SEND_STAGE_AWAITING_CONFIRM = 'awaiting_send_confirm';

// CustomIds — PREFIX-ONLY (no nonce). flow-dispatch's loadFlow already
// gates routing on stage + version; encoding identity into customId
// would not add safety (the dispatcher's trust model treats customId
// as a routing key, not an identity signal). Matches the convention
// REVOKE_SELECT_CUSTOM_ID / SETUP_BUTTON_CUSTOM_ID use.
//
// WIRE-PROTOCOL: the `qurl_confirm_*` literals below are encoded into
// every in-flight `flow_state` row for an open confirm card. Renaming
// them is a coordinated flip — wait for a SEND_FLOW_TTL_SECONDS (180s)
// drain on the prior deploy so in-flight rows expire before the new
// dispatcher routes against the new literal. See
// SEND_STAGE_AWAITING_CONFIRM above for the matching stage-value drain.
//
// The post-send Add Recipients / Revoke / Show Recipients buttons live on
// different customId prefixes (`qurl_add_*`, `qurl_revoke_*`,
// `qurl_expand_*`) — they are NOT affected by renaming the confirm-
// card literals here, so the 180s flow_state TTL is the only drain
// that needs to clear.
const CONFIRM_USER_SELECT_CUSTOM_ID = 'qurl_confirm_user_select';
const CONFIRM_SEND_CUSTOM_ID = 'qurl_confirm_send';
const CONFIRM_CANCEL_CUSTOM_ID = 'qurl_confirm_cancel';
// Confirm-card menus / button — slash options on /qurl send + /qurl map
// remain as initial defaults (one-shot for power users), but a user who
// didn't fill them in can still adjust expiry, self-destruct, and note
// inline on the card.
const CONFIRM_EXPIRY_SELECT_CUSTOM_ID = 'qurl_confirm_expiry';
const CONFIRM_SELF_DESTRUCT_SELECT_CUSTOM_ID = 'qurl_confirm_self_destruct';
const CONFIRM_NOTE_BUTTON_CUSTOM_ID = 'qurl_confirm_note_btn';
const CONFIRM_NOTE_MODAL_CUSTOM_ID = 'qurl_confirm_note_modal';
// Confirm-card "Everyone on voice" button. Rendered only
// when the slash command was invoked from a voice / stage-voice channel
// (`payload.voiceChannelId` is set by `handleQurlSlashSend`). Mirrors
// the role-mention/`<#voice>` parser path: resolve voice-connected
// non-bot members via `channel.members` AT CLICK TIME, not render time,
// so a 30s-old member snapshot doesn't silently send to people who
// left. PR #174's "voice-connected only" semantics are restored here
// (replacing the legacy multi-step `/qurl send` wizard's option,
// deleted in PR #313 alongside `getChannelMembers`. Note: the
// current `/qurl send` is the renamed `/qurl file` subcommand,
// NOT the deleted wizard — the name was reused after rename).
//
// STAGE-CHANNEL SEMANTICS: discord.js's `channel.members` for stage
// channels includes BOTH speakers and audience (everyone currently
// connected to the voice gateway in that stage). This is the
// "voice-connected" contract carried over from PR #174 and matches
// the legacy multi-step `/qurl send` wizard's behavior. A future stage-specific UX
// might want speakers-only (filter by `member.voice.suppress ===
// false`) — that's a separate affordance, not a regression in this
// path. Today the live `(N)` count on the button label is the user's
// best signal that a click against a 500-person stage will fan out
// to 500 DMs.
//
// NOT GATED ON MENTION_EVERYONE (intentional asymmetry with the
// @everyone slash-text path): the button is rendered only when the
// slash command was invoked from inside a voice channel, which
// intrinsically proves the sender's co-presence. A user already in
// a voice channel can DM each member individually; bundling the
// operation doesn't escalate privilege. Mirrors the same rationale
// at the parser `<#voice>` expansion site in recipient-parser.js.
// Tracked in #339 if a future product decision (stage channels with
// thousands of audience) needs to revisit this for stage-only.
const CONFIRM_VOICE_EVERYONE_BUTTON_CUSTOM_ID = 'qurl_confirm_voice_everyone';
// "Pick people instead" button — rendered in voice-mode and
// everyone-mode (i.e. `payload.recipientMode` is `'voice'` or
// `'everyone'`). Clicking it flips the card back to picker-mode
// (recipientIds dropped, MentionableSelect row restored). Lives on
// its own customId so flow-dispatch routes directly instead of
// branching inside the voice/@everyone handlers.
const CONFIRM_PICK_MANUAL_BUTTON_CUSTOM_ID = 'qurl_confirm_pick_manual';
// Whole-guild `@everyone` button on the confirm card. Workaround for
// Discord's MentionableSelectMenu omitting the `@everyone` role from
// its dropdown (platform UI filter), which would otherwise leave
// MENTION_EVERYONE-bearing users with no in-picker affordance for
// the "fan out to the whole server" intent. Click-time semantics
// match a synthetic `@everyone` text-mention pick. Picker-mode only —
// voice-mode already targets the voice population (different
// "everyone"), and everyone-mode is the post-click state where this
// button has already done its job (re-rendering it would invite a
// re-click that just churns the version with no state change).
const CONFIRM_EVERYONE_BUTTON_CUSTOM_ID = 'qurl_confirm_everyone';

// Three recipient-source modes carried on `payload.recipientMode`:
//   - 'picker' (default): MentionableSelect row is the source of
//     truth; bottom row carries the "🔊 Everyone on voice" affordance
//     when the slash command was invoked from voice, plus the
//     "📢 @everyone" affordance when the sender has MENTION_EVERYONE.
//   - 'voice': recipientIds were resolved from `channel.members` and
//     the picker row is hidden. Bottom row swaps in "👥 Pick people
//     instead" so the user can fall back to manual selection.
//   - 'everyone': recipientIds were resolved from the guild's full
//     non-bot member set via the 📢 @everyone shortcut. Like voice,
//     the picker row is hidden and the bottom row carries the
//     "👥 Pick people instead" escape hatch — auto-filling the
//     picker would either silently truncate at Discord's 25-entry
//     hard cap or read back through `handleConfirmUserSelect` and
//     replace the @everyone fan-out with a subset.
// Stale flow_state rows (created before this field existed) read as
// undefined — `normalizeRecipientMode` below maps undefined / any
// off-set value to RECIPIENT_MODE_PICKER so they keep the legacy
// picker shape until they expire.
const RECIPIENT_MODE_PICKER = 'picker';
const RECIPIENT_MODE_VOICE = 'voice';
const RECIPIENT_MODE_EVERYONE = 'everyone';

// Single source of truth for the closed-set map. Multiple render
// sites (renderConfirmCardContent, renderConfirmCardRows,
// rerenderConfirmCard) previously open-coded the ternary; an off-set
// or typo'd constant would silently fall through to PICKER and break
// the layout swap. The helper pins the stale-row default in one place.
function normalizeRecipientMode(mode) {
  if (mode === RECIPIENT_MODE_VOICE) return RECIPIENT_MODE_VOICE;
  if (mode === RECIPIENT_MODE_EVERYONE) return RECIPIENT_MODE_EVERYONE;
  return RECIPIENT_MODE_PICKER;
}

// Recipient-rejection reason strings shared by every handler that
// can drop a selection to empty (picker, voice-everyone). Hoisted
// from inline literals so a copy-edit (e.g., to "Cannot DM bots")
// updates every surface at once. Voice-specific warning copy is also
// hoisted here so all the user-visible strings live in one place
// rather than being scattered through 800+ lines of handler code.
const RECIPIENT_REASON_BOTS_DROPPED = 'Cannot send to bots';
const VOICE_REJECT_CHANNEL_UNREADABLE = `⚠\u{FE0F} Couldn't read the voice channel — it may have been deleted. Pick recipients below.\n\n`;
const VOICE_REJECT_EMPTY_CHANNEL = `⚠\u{FE0F} No one is connected to that voice channel right now. Pick recipients below.\n\n`;
const VOICE_REJECT_CONTEXT_LOST = `⚠\u{FE0F} Voice channel context was lost — use the picker below to choose recipients.\n\n`;
// Local to the note modal — kept off the prefix-only customId
// allowlist because flow-dispatch never routes modal-input fields,
// only the parent modal customId.
const SEND_NOTE_MODAL_FIELD_ID = 'message_value';

// 3-minute confirm-card window — the time-to-finish budget a user has
// to review the confirm card and click Send/Cancel after invoking
// /qurl send or /qurl map.
const SEND_FLOW_TTL_SECONDS = 180;

// Subcommands that require the guild API key resolution + cooldown gate.
// Single-source allowlist: adding a new send-style subcommand only
// requires touching this set, not the dispatcher fall-through.
//
// `'map'` joins the set only when MAP_COMMAND_ENABLED is on. The
// conditional removal is load-bearing — the gate runs BEFORE the
// dispatch in execute() below, so if `'map'` stayed in the set with
// the flag off, a stale-client /qurl map in an unconfigured guild
// would hit "qURL is not configured for this server" instead of
// QURL_MAP_DISABLED_REPLY.
const API_KEY_GATED_SUBCOMMANDS = new Set(
  config.MAP_COMMAND_ENABLED ? ['send', 'map', 'revoke'] : ['send', 'revoke'],
);

// User-facing reply for stale /qurl map submissions when the toggle
// is off. Reached when a Discord client routes a cached `map`
// submission after the registration has dropped it. Copy mirrors
// the flag-off branch of SETUP_SUCCESS_MSG so both surfaces describe
// the same remaining capability.
const QURL_MAP_DISABLED_REPLY = '❌ `/qurl map` is currently disabled. Use `/qurl send` to share files securely.';

// User-facing reply for stale /qurl detect submissions when DETECT_COMMAND_ENABLED
// is off — a Discord client routing a cached `detect` submission after the
// registration dropped it. Mirrors QURL_MAP_DISABLED_REPLY.
const QURL_DETECT_DISABLED_REPLY = '❌ `/qurl detect` is currently disabled.';

// Slash-option choice arrays. The same wording flows into both the
// slash-command autocomplete and the confirm-card dropdowns so users
// see consistent labels at every step. EXPIRY_CHOICES already exists.
// SELF_DESTRUCT_CHOICES is new here because the confirm card's
// self-destruct StringSelect mirrors a slash option, so both surfaces
// share one source of truth.
const SELF_DESTRUCT_NO_TIMER_CHOICE = 'none';
const SELF_DESTRUCT_CHOICES = [
  { name: 'No timer (default)', value: SELF_DESTRUCT_NO_TIMER_CHOICE },
  ...SELF_DESTRUCT_PRESETS.map((p) => ({ name: p.label, value: String(p.seconds) })),
];

// Map the slash option's string value to a seconds integer or null.
// Mirrors `selfDestructSelectValueToSeconds` in utils/time but for the
// slash-option value space (which uses 'none' instead of the form's
// SELF_DESTRUCT_NO_TIMER_VALUE).
//
// Defense-in-depth: validate against the SELF_DESTRUCT_PRESETS closed
// set the same way handleQurlSlashSend validates `expiresIn` against
// EXPIRY_LABELS. Discord enforces the choice set server-side, but a
// forged interaction could pass `'999999999'` and that value would
// otherwise land unchecked in the flow payload + connector upload.
// Wrong values fall back to null (no timer) so the failure is safe.
const SELF_DESTRUCT_PRESET_SECONDS = new Set(SELF_DESTRUCT_PRESETS.map((p) => p.seconds));
function selfDestructOptionToSeconds(value) {
  if (!value || value === SELF_DESTRUCT_NO_TIMER_CHOICE) return null;
  const n = Number(value);
  if (!isPositiveFinite(n)) return null;
  // Match the parsed Number directly — SELF_DESTRUCT_PRESETS contains
  // 0.5 (the "1/2 second" preset), so a Math.floor here would map 0.5
  // → 0 (not in the set) and silently downgrade to "no timer." The
  // form path (selfDestructSelectValueToSeconds in utils/time.js) uses
  // strict string equality against `String(p.seconds)` and gets this
  // right; matching the parsed numeric value against the Set keeps
  // the two entry points consistent.
  if (!SELF_DESTRUCT_PRESET_SECONDS.has(n)) return null;
  return n;
}

// Resolve a list of recipient IDs to User-shaped objects via the guild
// member cache (with fallback fetch on miss). Returns
// `{ users, unresolvedIds, transientFailureIds }` — callers surface
// each bucket with appropriate copy:
//   * `unresolvedIds` — Discord API 10007 "Unknown Member". The
//     canonical "user left the guild" signal. Stable / deterministic.
//   * `transientFailureIds` — any other fetch error (rate limit
//     surfacing past discord.js's retry, gateway blip, perms
//     revoked mid-fetch). User-visible copy should encourage retry
//     rather than imply the recipient is gone.
//
// Splitting the buckets prevents the 429-rendered-as-"left the
// server" misdirection.
//
// Fetches the cache-miss tail in parallel via `batchSettled` (the
// same helper used by the DM fan-out path; honors Discord's per-route
// rate-limit bucket by capping concurrent in-flight calls).
async function resolveRecipientUsers(interaction, ids) {
  if (!interaction.guild) {
    return { users: [], unresolvedIds: [...ids], transientFailureIds: [] };
  }
  const users = [];
  const missQueue = [];
  for (const id of ids) {
    const cached = interaction.guild.members.cache.get(id);
    if (cached) users.push(cached.user);
    else missQueue.push(id);
  }
  if (missQueue.length === 0) {
    return { users, unresolvedIds: [], transientFailureIds: [] };
  }

  const unresolvedIds = [];
  const transientFailureIds = [];
  const results = await batchSettled(missQueue, async (id) => {
    try {
      const m = await interaction.guild.members.fetch(id);
      return { id, user: m.user, kind: 'ok' };
    } catch (err) {
      // Discord error 10007 is "Unknown Member" — recipient really
      // left the guild. Anything else (429 rate-limit surfacing past
      // discord.js's retry, 500-class, gateway disconnect, perms
      // revoked) is transient and shouldn't read like "they're gone."
      const is10007 = err && (err.code === 10007 || err.code === '10007');
      if (!is10007) {
        logger.warn('resolveRecipientUsers: members.fetch failed (transient)', {
          recipient_id: id, error: err && err.message, code: err && err.code,
        });
      }
      return { id, user: null, kind: is10007 ? 'unknown_member' : 'transient' };
    }
  });
  for (const r of results) {
    // batchSettled wraps each callback in Promise.allSettled — fulfilled
    // results carry our `{id, user, kind}` shape. The callback never
    // throws (try/catch above) so a `rejected` status is unreachable in
    // practice; bucket as transient if it ever does fire.
    if (r.status !== 'fulfilled' || !r.value) continue;
    if (r.value.user != null) {
      users.push(r.value.user);
      continue;
    }
    if (r.value.kind === 'transient') transientFailureIds.push(r.value.id);
    else unresolvedIds.push(r.value.id);
  }
  return { users, unresolvedIds, transientFailureIds };
}

// Filter resolved Users: drop bots, detect whether the sender is
// included. Returns `{ valid, droppedBots, selfIncluded }` so the
// caller can surface "X bots were dropped" as a warning and "send
// includes yourself" as a neutral notice. Sender is NOT filtered by
// default — self-send is supported via the picker / text paths.
//
// `excludeSender: true` is the voice-everyone contract: the "Everyone
// in #voice" affordance is interpreted as "everyone else in the room",
// not "and CC myself". Sender is dropped pre-validity so `selfIncluded`
// is always false on that path (the content renderer's "Send includes
// you." notice would be misleading after exclusion).
//
// Known v1 cap-skew: parseRecipientMentions caps to QURL_SEND_MAX_RECIPIENTS
// BEFORE knowing which IDs are bots. A mention list of
// [bot1, bot2, ..., bot25, user1] with cap=25 yields ids=[bot1..bot25]
// and post-filter valid=[]. The user sees "all recipients dropped" and
// has to re-run with bots removed. Acceptable since (a) it requires a
// pathological mention list to trip, (b) the failure mode is loud
// rather than silent. Cost dimension worth flagging: 25 cache-miss
// bot mentions still hit `members.fetch` 25 times (via batchSettled's
// 5-at-a-time fan-out, in 5 rate-limit-budget burns) before partition
// drops them all — small absolute cost but burns per-guild rate-limit
// budget that legitimate ops elsewhere could use. The durable fix is
// the v2 resolve-then-cap refactor tracked in #304.
//
// Self-mention cap behavior: since self-send is supported, `<@me>` now
// CONSUMES a cap slot like any other mention. So `<@me> <@u1>..<@u25>`
// yields self + 24 others (was: 25 others, pre self-send). This is
// symmetric with the bot cap-skew above and aligns with the supported-
// recipient model — sender is just another recipient.
function partitionRecipients(users, senderId, { excludeSender = false } = {}) {
  // Dedup contract is OWNED upstream: parseRecipientMentions dedupes
  // via a Set (recipient-parser.js:197-198) and the UserSelectMenu
  // gateway-event surfaces each picked user at most once. This loop
  // therefore does NOT re-dedup; doing so would silently mask an
  // upstream regression (parser dedup breakage) that should fail
  // loudly via the recipient-count divergence test instead.
  const valid = [];
  let droppedBots = 0;
  let selfIncluded = false;
  for (const u of users) {
    if (u.bot) { droppedBots++; continue; }
    // Voice-everyone path drops the sender silently — they pressed
    // the "Everyone on voice" affordance, which semantically means
    // "everyone else." No droppedBots-style accounting because the
    // exclusion is inferred from voice-mode semantics rather than
    // surfaced in user-visible copy.
    if (u.id === senderId && excludeSender) continue;
    if (u.id === senderId) selfIncluded = true;
    valid.push(u);
  }
  return { valid, droppedBots, selfIncluded };
}

// Merge a MentionableSelectMenu pick (users + roles) into a deduped
// User[] for partitionRecipients. The `@everyone` role
// (role.id === guild.id) is gated on `canMentionEveryone` — same
// gate as the text-path #323. Non-mentionable role gating (#326)
// requires the same MENTION_EVERYONE permission (or `role.mentionable
// === true` as a per-role bypass) — Discord's picker filters this
// client-side, but a forged interaction would otherwise bypass it.
// Denied non-@everyone roles surface via `roleMentionsDenied:
// string[]` so the caller can render per-role copy with the name.
//
// Picked users seed `userMap` BEFORE role expansion so they get cap
// priority over role-expanded members (mirrors the text-path #323
// round-4 fix).
//
// For the @everyone role specifically, iterate `guild.members.cache`
// instead of `role.members` — discord.js doesn't reliably surface
// all members through @everyone's role.members.
//
// `droppedFromRoles` is the count of DISTINCT bot-user IDs filtered
// during role expansion (Set-backed) — matches what the user sees
// rendered as "N bot(s) filtered from picked role(s)." A bot that
// appears in two picked roles, or is directly picked AND also in a
// picked role, is counted at most once.
//
// Map/Collection compatibility: `interaction.users` and
// `interaction.roles` are discord.js Collections (which extend Map)
// in prod and plain Maps in tests. Both implement `.values()`,
// `.entries()`, and `Symbol.iterator` — the duck-type guards below
// gate on the methods we actually call.
function resolveMentionableSelection({ interaction, canMentionEveryone, flow_id = null }) {
  const guild = interaction.guild;
  const userMap = new Map();
  if (interaction.users && typeof interaction.users.values === 'function') {
    for (const u of interaction.users.values()) {
      if (u && u.id) userMap.set(u.id, u);
    }
  }
  let massMentionDenied = false;
  // Set when the user picked @everyone WITH MENTION_EVERYONE but the
  // guild.members cache is missing or empty — e.g. immediately after
  // a bot restart, before the lazy-load fills it. Lets the caller
  // surface a "try again in a few seconds" reason instead of the
  // silent deferUpdate-only no-op the user-visible path would
  // otherwise hit.
  let everyoneCacheCold = false;
  // Per-role denied list (issue #326): a picked role that's not
  // `mentionable: true` AND the sender lacks MENTION_EVERYONE lands
  // here as `roleId`. Dedup via parallel Set so a future picker that
  // surfaces the same role twice doesn't double-render. Tracked as
  // IDs; the caller resolves names via `guild.roles.cache.get(id)
  // ?.name` so renderRecipientWarnings stays pure (no guild
  // dependency).
  const roleMentionsDenied = [];
  const roleMentionsDeniedIds = new Set();
  // Distinct bot IDs filtered across all picked roles — caller renders
  // .size as "N bot(s) filtered." Tracking as a Set (not a counter)
  // so overlap (bot in two roles, or directly-picked bot also in a
  // role) doesn't inflate the user-visible number.
  const droppedFromRolesSet = new Set();
  // Raw inner-loop iterations across all picked roles. Bounds the
  // iteration COST independent of unique-bot semantics, so a
  // pathological 10k-entry role can't grind for free.
  let inspectedFromRoles = 0;
  // Gate the ITER_BOUND debug log so it fires AT MOST ONCE per call.
  // The counter is function-scoped, so without this gate role B's
  // inner loop would log a redundant line on every iteration past
  // role A's budget exhaustion. Forensic value is one line per call,
  // not N.
  let boundLogged = false;
  // 4× cap (=100) balances pathological-role protection against the
  // UX edge where a real role iterates bots first and humans behind:
  // 2× was tight enough that a "bots-up-front" role could leave the
  // user under-expanded with no visible signal beyond a high count.
  // 4× pushes the rate-of-occurrence well below any plausible
  // non-pathological role layout.
  const ITER_BOUND = 4 * config.QURL_SEND_MAX_RECIPIENTS;
  if (interaction.roles && typeof interaction.roles.entries === 'function') {
    for (const [roleId, role] of interaction.roles.entries()) {
      const isEveryoneRole = guild && roleId === guild.id;
      if (isEveryoneRole && !canMentionEveryone) {
        // Surfaced even if userMap is already at cap — the gate firing
        // is a user-visible signal worth showing regardless of whether
        // expansion would have added anyone.
        massMentionDenied = true;
        continue;
      }
      // Skip undefined-role entries (theoretical — Discord's picker
      // surfaces roles via `interaction.roles.entries()`, which should
      // always carry the Role object alongside the ID; but a partial
      // fetch shape could deliver a bare ID). Gate placement parallels
      // the text-path parser's `if (!role) { pushInvalidIfNew(...) }`
      // branch — without this short-circuit, the `role?.mentionable
      // !== true` gate below would route a cache-miss role through
      // the deny path and surface "Non-mentionable role" copy for
      // what's actually a missing object. RESIDUE DIVERGES BY DESIGN:
      // the parser surfaces invalid-role IDs in `invalidTokens` so
      // the user sees a "couldn't parse" bullet, but the picker has
      // no parse-error context to surface (the picker submitted IDs
      // are by definition Discord-rendered choices), so silent
      // `continue` is the right residue here.
      if (!isEveryoneRole && !role) continue;
      // Per-role MENTION_EVERYONE gate (issue #326), parallel to the
      // text-path gate in recipient-parser.js. Discord's picker filters
      // non-mentionable roles client-side for users without the perm —
      // this gate is defense-in-depth against a forged interaction or
      // a future client-side filter regression. The @everyone-role
      // branch above already handled (isEveryoneRole), so this gate
      // only fires for non-@everyone roles. `role.mentionable === true`
      // is the per-role bypass (set explicitly by a role owner).
      if (!isEveryoneRole && role.mentionable !== true && !canMentionEveryone) {
        if (!roleMentionsDeniedIds.has(roleId)) {
          roleMentionsDeniedIds.add(roleId);
          roleMentionsDenied.push(roleId);
        }
        continue;
      }
      // `guild` is provably truthy in the @everyone branch (gated by
      // `guild && roleId === guild.id` above).
      const source = isEveryoneRole
        ? guild.members?.cache
        : role?.members;
      // Two distinct cold-cache shapes both surface the same UX
      // signal (everyoneCacheCold):
      //   - `source` undefined / non-iterable: discord.js hasn't
      //     created the cache slot yet (e.g., `guild.members = {}`
      //     immediately post-restart).
      //   - `source.size === 0`: cache slot exists but no members
      //     populated yet (e.g., between bot ready and
      //     chunk-on-startup completion).
      if (!source || typeof source.entries !== 'function') {
        if (isEveryoneRole) everyoneCacheCold = true;
        continue;
      }
      if (isEveryoneRole && source.size === 0) {
        everyoneCacheCold = true;
        continue;
      }
      // Two break conditions, both belt-and-suspenders:
      //  - userMap.size at cap: stops adding new entries once the
      //    downstream partition cap would be hit anyway.
      //  - inspectedFromRoles past 4× cap: bounds iteration cost
      //    against a pathological all-bot role where the size guard
      //    can never fire. Logs at debug so a user-reported "I picked
      //    a role and got fewer than expected" has a forensic hook.
      //
      // The counter is incremented BEFORE the dedupe checks below, so
      // a bot in two picked roles costs 2 iteration slots even though
      // it lands in droppedFromRolesSet once. That's intentional —
      // the bound governs CPU cost (entries inspected), not the
      // user-visible count semantic. Hoisting the dedupe above the
      // counter would let a contrived "same 100 members across N
      // roles" pick grind for free.
      for (const [memberId, member] of source.entries()) {
        if (userMap.size >= config.QURL_SEND_MAX_RECIPIENTS) break;
        if (inspectedFromRoles >= ITER_BOUND) {
          if (!boundLogged) {
            logger.debug('resolveMentionableSelection: ITER_BOUND hit during role expansion', {
              flow_id,
              role_id: roleId,
              inspected_from_roles: inspectedFromRoles,
              user_map_size: userMap.size,
              iter_bound: ITER_BOUND,
            });
            boundLogged = true;
          }
          break;
        }
        inspectedFromRoles += 1;
        // Defense: partial GuildMember objects from sparse fetches
        // could carry an undefined `.user`. Downstream partitionRecipients
        // would deref `.bot` / `.id` on undefined; skip such entries
        // up front. Symmetric with the picked-users seed guard above.
        if (!member?.user?.id) continue;
        // Dedupe BEFORE the bot check: a directly-picked bot is
        // already in userMap (seeded above), and partitionRecipients
        // will report it via droppedBots. Skipping here means it
        // doesn't ALSO tick the role-side bot signal — avoids
        // surfacing two warnings for the same underlying bot.
        if (userMap.has(memberId)) continue;
        if (droppedFromRolesSet.has(memberId)) continue;
        if (isBotMember(member)) {
          droppedFromRolesSet.add(memberId);
          continue;
        }
        // member.user is kept here (not the picker's User object)
        // because picked-user seeding already happened; field fidelity
        // for picked users is preserved by the userMap.has skip above.
        userMap.set(memberId, member.user);
      }
    }
  }
  return {
    users: [...userMap.values()],
    massMentionDenied,
    droppedFromRoles: droppedFromRolesSet.size,
    everyoneCacheCold,
    roleMentionsDenied,
  };
}

/**
 * Render a warnings block for the confirm card. Returns the empty
 * string when nothing is worth surfacing — keeps callers simple
 * (`warningsBlock + content`) without a separate "do I have any
 * warnings" check.
 *
 * COPY PARITY: the partial-valid pick path renders bulleted lines
 * from here; the all-invalid pick path in handleConfirmUserSelect
 * builds its own joined-sentence reasons list with shorter copy
 * (rejection banner vs. warnings block — different UI contexts).
 * Both surface the SAME set of signals (droppedBots,
 * droppedFromRoles, massMentionDenied, everyoneCacheCold,
 * roleMentionsDeniedNames). When adding or renaming a signal, update
 * BOTH surfaces in lockstep — the helpers don't share a copy table,
 * so a future contributor could easily land one half and not the
 * other.
 *
 * `roleMentionsDeniedNames` is a pre-resolved list of role NAMES
 * (caller maps role IDs through `guild.roles.cache.get(id)?.name`,
 * falling back to a placeholder for cache-miss / deleted roles).
 * Keeping the helper pure of guild lookups mirrors how
 * `invalidTokens` arrives pre-formatted from the parser. Names are
 * truncated AT RENDER time here: each name is capped at
 * WARNING_NAME_CODEPOINT_CAP (80) codepoints with backticks stripped,
 * and the listed count is capped at WARNING_LIST_DISPLAY_MAX (10)
 * with a tail-count line — callers don't need to pre-sanitize.
 *
 * @param {{
 *   invalidTokens?: string[],
 *   cappedCount?: number,
 *   unresolvedIds?: string[],
 *   transientFailureIds?: string[],
 *   droppedBots?: number,
 *   droppedFromRoles?: number,
 *   massMentionDenied?: boolean,
 *   everyoneCacheCold?: boolean,
 *   roleMentionsDeniedNames?: string[],
 * }} [opts]
 */
function renderRecipientWarnings({
  invalidTokens = [],
  cappedCount = 0,
  unresolvedIds = [],
  transientFailureIds = [],
  droppedBots = 0,
  droppedFromRoles = 0,
  massMentionDenied = false,
  everyoneCacheCold = false,
  roleMentionsDeniedNames = [],
} = {}) {
  const lines = [];
  // Shared sanitizer for user-/admin-controlled strings rendered inline
  // in a bullet: strip backticks (prevent code-fence breakout) then
  // codepoint-slice with ellipsis. Used by both the invalidTokens and
  // roleMentionsDeniedNames bullets — single-sourced so the two paths
  // can't drift.
  const sanitizeForBullet = (s) =>
    sliceWithEllipsis(s.replace(/`/g, ''), WARNING_NAME_CODEPOINT_CAP, '…');
  if (cappedCount > 0) {
    lines.push(`• Capped at ${config.QURL_SEND_MAX_RECIPIENTS} — ${cappedCount} recipient(s) past the cap were dropped.`);
  }
  if (invalidTokens.length > 0) {
    // recipient-parser.js's docstring is explicit: invalidTokens are
    // NOT markdown-escaped — callers wrap or escape. We code-fence
    // for literal rendering, but a token containing ``` would close
    // the fence early and inject markdown / masked-links into the
    // ephemeral. Strip backticks from each token before joining;
    // also cap the listed count at 10 so a pathological list can't
    // blow the Discord content budget.
    //
    // Per-token codepoint cap (WARNING_NAME_CODEPOINT_CAP, 80) keeps
    // the worst case bounded: recipient-parser.js caps each token at
    // 256 chars, so 10 × 256 = 2.5KB of code-fenced text would dwarf
    // the rest of the card and risk crowding out the action prompt.
    // 80 codepoints is enough to spot the typo / paste error without
    // rendering the attacker's entire payload. List cap
    // (WARNING_LIST_DISPLAY_MAX, 10) bounds the bullet count; both
    // constants are shared with the role-mentions-denied path below.
    const shown = invalidTokens.slice(0, WARNING_LIST_DISPLAY_MAX).map(sanitizeForBullet);
    const more = invalidTokens.length > WARNING_LIST_DISPLAY_MAX
      ? ` (+${invalidTokens.length - WARNING_LIST_DISPLAY_MAX} more)`
      : '';
    lines.push('• Could not parse:\n```\n' + shown.join('\n') + '\n```' + more);
  }
  if (unresolvedIds.length > 0) {
    lines.push(`• ${unresolvedIds.length} user(s) are no longer in this server and were dropped.`);
  }
  if (transientFailureIds.length > 0) {
    // Distinct copy from unresolvedIds — transient failures (429,
    // gateway blip) mislead the user if rendered as "they left the
    // server." Encourage retry.
    lines.push(`• ${transientFailureIds.length} user(s) couldn't be looked up right now — try again in a moment.`);
  }
  if (droppedBots > 0) {
    lines.push(`• ${droppedBots} bot(s) cannot receive qURL links — skipped.`);
  }
  if (droppedFromRoles > 0) {
    // Surfaced symmetrically with droppedBots so the user knows the
    // role pick was partially filtered (vs. silently shrinking the
    // recipient count). Distinct copy from droppedBots since the
    // user took a different picker action (selecting a role rather
    // than an individual bot).
    lines.push(`• ${droppedFromRoles} bot(s) filtered from picked role(s) — skipped.`);
  }
  if (everyoneCacheCold) {
    // Symmetric with droppedFromRoles: surfaced even on partial-valid
    // picks (named user + @everyone with cold cache → user lands but
    // @everyone silently expanded to zero, which the user would
    // otherwise have no way to know).
    lines.push('• Member cache not yet ready — `@everyone` expanded to 0 members. Try again in a few seconds.');
  }
  if (massMentionDenied) {
    // Specific copy beats the generic "couldn't parse" path so the
    // user knows it's a PERMISSION issue, not a typo. Mirrors
    // Discord's own MENTION_EVERYONE gate. The caller suppresses
    // this in DM context (where @everyone has no meaning) so the
    // copy here doesn't need a context qualifier.
    lines.push('• `@everyone` requires the **Mention Everyone** permission — skipped.');
  }
  if (roleMentionsDeniedNames.length > 0) {
    // One bullet per denied role so the user can tell which mention
    // tripped the gate (the @everyone bullet above doesn't enumerate
    // because it's always a single semantic action).
    // WARNING_LIST_DISPLAY_MAX (10) bounds the embed footprint under
    // a forged-interaction attempt to enumerate every guild role;
    // tail-count discloses the rest.
    //
    // Sanitization mirrors the invalidTokens path above:
    //   - inline code fence (single backtick) so a role name
    //     containing Discord markdown renders literally;
    //   - backticks inside the name stripped to prevent fence
    //     breakout;
    //   - codepoint-aware slice via sliceWithEllipsis
    //     (WARNING_NAME_CODEPOINT_CAP, 80) so a 100-codepoint role
    //     name can't dwarf the rest of the card. Role names are
    //     admin-controlled but Discord doesn't restrict the character
    //     set, so treat the input as untrusted for rendering.
    const shown = roleMentionsDeniedNames.slice(0, WARNING_LIST_DISPLAY_MAX);
    for (const name of shown) {
      lines.push(`• \`@${sanitizeForBullet(name)}\` requires the **Mention Everyone** permission or \`role.mentionable: true\` — skipped.`);
    }
    if (roleMentionsDeniedNames.length > WARNING_LIST_DISPLAY_MAX) {
      lines.push(`• (+${roleMentionsDeniedNames.length - WARNING_LIST_DISPLAY_MAX} more role mention(s) skipped.)`);
    }
  }
  if (lines.length === 0) return '';
  return '⚠\u{FE0F} **Some recipients were dropped:**\n' + lines.join('\n') + '\n\n';
}

// Build the confirm-card content string. Brand spelling is `qURL`
// per CLAUDE.md — the user-visible copy here, the slash-command
// descriptions, and any logger.audit user-facing fields all preserve
// the case.
function renderConfirmCardContent({
  resourceType, resourceLabel, validRecipients,
  expiresIn, selfDestructSeconds, personalMessage,
  warningsBlock, needsPicker, interaction, selfIncluded = false,
  recipientMode, voiceChannelId,
}) {
  let content = warningsBlock || '';
  const mode = normalizeRecipientMode(recipientMode);
  // Explicit branch per RESOURCE_TYPES value — a future resource
  // type (audio, contact card, etc.) MUST add its own branch here.
  // Throwing on unknown beats silently rendering the new type as a
  // location, which would propagate the misdirection into every
  // confirm card the new type ever shows.
  if (resourceType === RESOURCE_TYPES.FILE) {
    content += `📁 **Sending file:** ${resourceLabel}\n`;
  } else if (resourceType === RESOURCE_TYPES.MAPS) {
    content += `🗺\u{FE0F} **Sending location:** ${resourceLabel}\n`;
  } else {
    throw new TypeError(`renderConfirmCardContent: unknown resourceType ${JSON.stringify(resourceType)}`);
  }
  // Neutral notice (NOT a warning) when the sender is in the recipient
  // list. Self-send is supported as a first-class flow; surface it on
  // the confirm card so the user can verify they meant to include
  // themselves before clicking Send.
  //
  // Mode guard: every voice-mode write path explicitly sets
  // `selfIncluded: false` (slash-entry override + handleConfirmVoiceEveryone
  // + handleConfirmPickManual), so `selfIncluded === true` is
  // structurally unreachable in voice-mode. The guard is defense
  // against a forged or schema-drifted payload that would otherwise
  // produce contradictory copy ("Send includes you." stacked on top
  // of a voice-mode "To:" line whose semantics exclude the sender).
  if (selfIncluded && mode !== RECIPIENT_MODE_VOICE) {
    content += 'ℹ\u{FE0F} **Send includes you.**\n';
  }
  if (needsPicker) {
    content += '\n**Pick recipients below.**\n';
  } else if (mode === RECIPIENT_MODE_VOICE) {
    // Voice-mode "To:" — names omitted in favor of the channel context.
    // The #voice mention is the source of truth for who's included;
    // listing alias names would just duplicate the scrollback the user
    // already sees in Discord. Sender exclusion is left inferred from
    // voice-mode semantics (the user clicked "Everyone on voice" knowing
    // they themselves wouldn't qURL themselves) rather than spelled out
    // — the explicit "(you not included)" disclosure dropped per UX
    // call; selfIncluded:false is still load-bearing on the data side.
    //
    // SAFETY: use Discord's native channel-mention syntax `<#id>` (which
    // Discord renders client-side from the channel id) instead of
    // interpolating `channel.name` raw. A name like `**spoiler**`,
    // `_underline_`, or `||hidden||` would otherwise inject markdown
    // into the confirm card. Mirrors the same gotcha noted at the
    // voice-button label site (where button-label rendering doesn't
    // process markdown, but message content does).
    const channelRef = voiceChannelId ? `<#${voiceChannelId}>` : 'the voice channel';
    const userWord = validRecipients.length === 1 ? 'user' : 'users';
    content += `\n**To:** ${validRecipients.length} ${userWord} in ${channelRef}\n`;
  } else {
    // First-N preview keeps the card scannable when a paste resolves
    // to many users. resolveRecipientAlias prefers nickname > globalName >
    // username (NFKC + bidi/zero-width strip baked in), matching the
    // post-send confirmation wording exactly. The escapeDiscordMarkdown
    // wrap is still required — resolveRecipientAlias returns the PLAIN
    // form (its docstring at commands.js:~305 calls this out).
    const PREVIEW = 5;
    const previewNames = validRecipients.slice(0, PREVIEW)
      .map((u) => escapeDiscordMarkdown(resolveRecipientAlias(u, interaction)))
      .join(', ');
    const more = validRecipients.length > PREVIEW ? `, +${validRecipients.length - PREVIEW} more` : '';
    content += `\n**To:** ${validRecipients.length} user${validRecipients.length === 1 ? '' : 's'} (${previewNames}${more})\n`;
  }
  content += `**Expires:** ${EXPIRY_LABELS[expiresIn] || expiresIn}\n`;
  if (isPositiveFinite(selfDestructSeconds)) {
    content += `**Self-destruct:** ${formatSelfDestructLabel(selfDestructSeconds)}\n`;
  }
  if (personalMessage) {
    // `personalMessage` is pre-sanitized: sanitizeMessage already
    // ran markdown-escape + @-mention strip at the slash-option
    // boundary. Re-escaping here would render `\*\*bold\*\*`
    // literals on the card.
    //
    // Quote-block syntax `> ` (instead of `"..."` wraps) avoids the
    // ragged-look failure mode when the message itself contains a
    // literal `"` character. Discord renders `> ` as a left-bar
    // blockquote which visually offsets the preview from the rest
    // of the card.
    content += `**Note:** ${formatPersonalMessagePreview(personalMessage)}\n`;
  }
  content += '\nClick **Send** to deliver one-time qURL, or **Cancel** to abort.';
  // Fail-safe cap below Discord's 2000-char content limit so adversarial
  // inputs can't trip a 400 from editReply and orphan the flow row.
  // Cap (1988) + '…(truncated)' indicator (12 codepoints) = 2000 max
  // for ASCII / BMP content. Surrogate-pair-stuffed input could still
  // exceed 2000 UTF-16 units, but every contributing field
  // (`resourceLabel`, display-name aliases, personalMessage preview)
  // is already codepoint-capped upstream, so that path is exotic.
  return sliceWithEllipsis(content, 1988, '…(truncated)');
}

// Truncate the pre-sanitized message to 80 chars MAX, then back off
// to the last completed markdown-escape so a slice doesn't leave a
// lone trailing `\`. Embedded `\n` is collapsed to a single space —
// Discord blockquotes are per-LINE (only the line starting with `> `
// gets the left-bar), so a multi-line message would render with a
// quoted first line and flush-left subsequent lines. The single-line
// preview also keeps the card's vertical rhythm predictable.
//
// The "back off" handles the case where `sanitizeMessage` emitted a
// `\` immediately before the slice boundary (e.g. `\*` becomes `\`
// at index 79, `*` at index 80 — slicing at 80 leaves a dangling
// `\` that Discord would render as a literal backslash). Trimming
// to index 79 in that case is the conservative fix.
// Newlines and Unicode line/paragraph separators (U+2028, U+2029) all
// render as line breaks in Discord — collapse the lot to single spaces
// so the blockquote preview stays a single rendered line. Built via
// `new RegExp(...)` (instead of a literal) so the \uXXXX escapes go
// through string parsing — keeps the source ASCII-only and avoids
// editor/tool-chain confusion over raw line-separator codepoints in a
// regex literal. Same pattern STRIP_RE in utils/sanitize.js uses.
const NEWLINE_COLLAPSE_RE = new RegExp('[\\r\\n\\u2028\\u2029]+', 'g');

function formatPersonalMessagePreview(message) {
  const oneLine = message.replace(NEWLINE_COLLAPSE_RE, ' ');
  // safeCodepointSlice handles surrogate-safe truncation + odd-trailing-
  // backslash backoff. The early-return at 80 codepoints avoids the
  // `…` ellipsis when there's nothing to truncate.
  //
  // Caveat: codepoint-aware != grapheme-aware. ZWJ-joined emoji
  // sequences (e.g. man + ZWJ + woman + ZWJ + girl, three
  // codepoints + two joiners = 5 codepoints) can be sliced mid-cluster
  // and render only the first segment. Acceptable: the preview is
  // an 80-codepoint truncation indicator (followed by `…`), so a
  // partial emoji renders as exactly that — a partial display, not
  // garbled text. Full grapheme-segmentation would need Intl.Segmenter
  // which isn't justified for a preview cap.
  if (Array.from(oneLine).length <= 80) return `> ${oneLine}`;
  return `> ${safeCodepointSlice(oneLine, 80)}…`;
}

// Build the ActionRow set for the confirm card. Layout depends on
// `recipientMode` — each select needs its own row (Discord forbids
// selects + buttons in the same ActionRow).
//
// MentionableSelect (per #328) surfaces users AND roles in one menu;
// role picks expand in handleConfirmUserSelect via
// resolveMentionableSelection with @everyone-role MENTION_EVERYONE
// gating (same gate as the text-path #323).
// CONFIRM_USER_SELECT_CUSTOM_ID wire literal kept — renaming would
// need the 180s flow_state drain coordination from #316.
//
// Row math (Discord 5-row max, 5-buttons-per-row max):
//   PICKER mode:   Mentionable + SelfDestruct + Expiry +
//     [(optional 🔊 Voice), (optional 📢 @everyone), Note, Send, Cancel]
//     = 4 rows, ≤ 5 buttons
//   VOICE mode:    SelfDestruct + Expiry +
//     [👥 Pick people instead, Note, Send, Cancel]   = 3 rows, 4 buttons
//   EVERYONE mode: SelfDestruct + Expiry +
//     [👥 Pick people instead, Note, Send, Cancel]   = 3 rows, 4 buttons
//
// The bottom-row affordance flips with the mode:
//   - `'picker'` + voiceChannelId set → 🔊 Everyone on voice button.
//     Disabled with a `(0)` count when the channel has no connected
//     non-bot members at render time — honest UX so the user can see
//     WHY the button is inert.
//   - `'voice'` or `'everyone'` → 👥 Pick people instead button.
//     Picker row is removed entirely; the user has committed to a
//     bulk-select semantics and this button is their escape hatch
//     back to manual selection. Both modes share a single handler
//     (handleConfirmPickManual) that resets recipientMode to 'picker'.
// The actual member resolution still happens at click time (see
// `handleConfirmVoiceEveryone` and `handleConfirmEveryone`); the
// counts here are snapshot hints so the labels aren't blank.
//
// `recipientMode` defaults to RECIPIENT_MODE_PICKER for stale rows that
// existed before this field was introduced; the legacy shape is exactly
// the picker-mode branch below.

// Memoized non-bot count for the @everyone button label. Confirm cards
// re-render on every picker change / expiry select / note add+edit; in
// a single flow the cache typically doesn't change between re-renders,
// so iterating it on every render is wasted work. The WeakMap is keyed
// by `guild` and entries auto-evict when the Guild is GC'd.
//
// FRESHNESS FINGERPRINT: `cache.size:memberCount` busts the memo when
// either changes (the common case for member join/leave). KNOWN GAP:
// a swap event — one human leaves and one bot joins near-simultaneously
// — keeps both values stable but flips the non-bot count by 1. The
// memo would return the stale count until the next non-swap churn.
// Acceptable because (a) the label is documented as a hint, not
// authoritative, and (b) the click-time `partitionRecipients` is the
// source of truth. Folding a churn signal into the fingerprint would
// be cheap if this ever needs tightening.
//
// Returns `{ count, accurate }`:
//   - `accurate: true` when the warm-cache branch fired (cache fully
//     populated AND bots filtered) — safe to gate render-time disables
//     on the count
//   - `accurate: false` for the cold-cache fallback (`memberCount`
//     includes bots — over-counts by bot population, so a guild near
//     the cap could false-positive on over-cap disable)
//   - `count: null` when neither branch can produce a number
const everyoneCountMemo = new WeakMap();
function computeEveryoneDisplayCount(guild) {
  const cache = guild?.members?.cache;
  // Display-only path — `approximateMemberCount` fallback is acceptable
  // here even though prewarmGuildMembersCache deliberately avoids it.
  // A render-time label off by a few members is benign; the strict gate
  // is reserved for the prewarm decision where underresolve would
  // silently truncate the @everyone recipient set.
  const memberCount = effectiveGuildMemberCount(guild);
  if (cache && typeof cache.size === 'number' && memberCount != null && cache.size >= memberCount) {
    const fingerprint = `${cache.size}:${memberCount}`;
    const cached = everyoneCountMemo.get(guild);
    if (cached && cached.fingerprint === fingerprint) return { count: cached.count, accurate: true };
    let n = 0;
    for (const [, m] of cache) {
      // `m?.user &&` aligns the render-time label with the click-time
      // filter in handleConfirmEveryone — that path tallies rows
      // without `.user` as `partialCacheDrops` and excludes them from
      // recipients. Without the guard, a degraded GUILD_MEMBERS_CHUNK
      // row counts as a non-bot here (isBotMember reads `.user?.bot`,
      // returning false on missing `.user`) but never reaches the
      // recipient set, leaving the label over-stated.
      if (m?.user && !isBotMember(m)) n++;
    }
    everyoneCountMemo.set(guild, { fingerprint, count: n });
    return { count: n, accurate: true };
  }
  if (typeof memberCount === 'number') return { count: memberCount, accurate: false };
  return { count: null, accurate: false };
}

function renderConfirmCardRows({
  sendDisabled, expiresIn, selfDestructSeconds, personalMessage,
  voiceChannelId, interaction, recipientIds, recipientMode,
}) {
  const rows = [];
  const mode = normalizeRecipientMode(recipientMode);
  // Pre-resolved recipientIds past maxValues stay in payload.recipientIds
  // and still get sent — only the visual pre-check via addDefaultUsers
  // truncates.
  const defaults = Array.isArray(recipientIds) ? recipientIds : [];
  const maxValues = Math.min(
    USER_SELECT_PER_PICK_CAP,
    config.QURL_SEND_MAX_RECIPIENTS,
    DISCORD_SELECT_MAX_VALUES_HARD_CAP,
  );
  if (mode === RECIPIENT_MODE_PICKER) {
    const picker = new MentionableSelectMenuBuilder()
      .setCustomId(CONFIRM_USER_SELECT_CUSTOM_ID)
      .setPlaceholder(`Pick up to ${maxValues} ${maxValues === 1 ? 'user/role' : 'users/roles'}`)
      .setMinValues(1)
      .setMaxValues(maxValues);
    if (defaults.length > 0) {
      // Text-path recipientIds are user IDs only — parseRecipientMentions
      // expands roles to members before partition. addDefaultUsers is the
      // matching API on MentionableSelectMenuBuilder (addDefaultRoles
      // would be wrong here — we never persist role IDs).
      picker.addDefaultUsers(...defaults.slice(0, maxValues));
    }
    rows.push(new ActionRowBuilder().addComponents(picker));
  }
  // Self-destruct StringSelectMenu: "No self-destruct timer" + 7
  // curated presets sourced from SELF_DESTRUCT_PRESETS in utils/time.
  // The `default: true` flag on the matching option keeps the
  // collapsed-header text reflective of the current value across
  // re-renders. `hasTimer` + `hasMatchingPreset` defend against a
  // corrupted DDB row carrying an off-preset finite value (which would
  // otherwise leave every option un-defaulted and force Discord to
  // render the first option's label — wrong UX).
  const hasTimer = isPositiveFinite(selfDestructSeconds);
  const hasMatchingPreset = hasTimer && SELF_DESTRUCT_PRESETS.some((p) => p.seconds === selfDestructSeconds);
  if (hasTimer && !hasMatchingPreset) {
    // Symmetric with the expiry off-set warn below: a finite positive
    // selfDestructSeconds that misses every preset means a corrupted
    // row. The "No timer" fallback keeps the header sensible but the
    // content line still renders "self-destructs in N seconds" — log
    // so the asymmetric state surfaces in forensics.
    logger.warn('renderConfirmCardRows: off-preset selfDestructSeconds in payload — falling back to No timer default', {
      selfDestructSeconds: truncForLog(selfDestructSeconds),
    });
  }
  rows.push(new ActionRowBuilder().addComponents(
    new StringSelectMenuBuilder()
      .setCustomId(CONFIRM_SELF_DESTRUCT_SELECT_CUSTOM_ID)
      .setMinValues(1)
      .setMaxValues(1)
      .addOptions(
        { label: 'No self-destruct timer', value: SELF_DESTRUCT_NO_TIMER_VALUE, default: !hasMatchingPreset },
        ...SELF_DESTRUCT_PRESETS.map((p) => ({
          label: `\u{1F4A5} ${p.label}`,
          value: String(p.seconds),
          default: hasMatchingPreset && selfDestructSeconds === p.seconds,
        }))
      )
  ));
  // Expiry StringSelectMenu. Default-true on the matching option same
  // as self-destruct above. EXPIRY_CHOICES is shared with the slash-
  // option choice list so users see identical labels in autocomplete
  // and in the form. `hasExpiryMatch` defends against a corrupted DDB
  // row carrying an off-`EXPIRY_LABELS` value — without it every
  // option would be un-defaulted and Discord renders the first
  // option's label, misrepresenting the actual stored value. Falls
  // back to defaulting the codebase-default '24h' option so the card
  // still shows SOMETHING meaningful.
  const hasExpiryMatch = isValidExpiry(expiresIn);
  if (!hasExpiryMatch && expiresIn != null) {
    // Log so corrupted rows surface in forensics rather than just
    // getting papered over by the 24h fallback.
    logger.warn('renderConfirmCardRows: off-EXPIRY_LABELS expiresIn in payload — falling back to 24h default', {
      expiresIn: truncForLog(expiresIn),
    });
  }
  rows.push(new ActionRowBuilder().addComponents(
    new StringSelectMenuBuilder()
      .setCustomId(CONFIRM_EXPIRY_SELECT_CUSTOM_ID)
      .setMinValues(1)
      .setMaxValues(1)
      .addOptions(
        ...EXPIRY_CHOICES.map((c) => ({
          label: c.name,
          value: c.value,
          default: hasExpiryMatch ? c.value === expiresIn : c.value === '24h',
        }))
      )
  ));
  // Bottom row: [optional 🔊 Voice | 👥 Pick people instead] + Note +
  // Send + Cancel. Note label flips between "Add a note" and "Edit
  // note" based on current state so the user can tell at a glance
  // whether a note is attached. The leading-button affordance is
  // mode-dependent: picker-mode shows the voice-everyone entry button
  // (only when the slash was invoked from voice); voice-mode and
  // everyone-mode show the "Pick people instead" escape hatch.
  const bottomRow = new ActionRowBuilder();
  // Voice-mode and everyone-mode share the same escape hatch: picker
  // row is hidden above, so this button is the only path back to
  // manual selection without re-running the slash command. Style as
  // Secondary so it doesn't compete visually with Send (Success).
  // Handler is shared (handleConfirmPickManual) and resets
  // recipientMode → 'picker'.
  //
  // DEFENSIVE: voice-mode gate does NOT require voiceChannelId.
  // Every production write path pairs voice-mode with voiceChannelId,
  // but a forged or schema-drifted payload presenting voice-mode
  // without it would otherwise fall through to the picker-no-voice
  // branch below and render the 📢 @everyone entry button against a
  // hidden picker — no recovery path. Closing the gate here keeps
  // the escape hatch reachable in that degraded state.
  if (mode === RECIPIENT_MODE_VOICE || mode === RECIPIENT_MODE_EVERYONE) {
    bottomRow.addComponents(
      new ButtonBuilder()
        .setCustomId(CONFIRM_PICK_MANUAL_BUTTON_CUSTOM_ID)
        .setLabel('\u{1F465} Pick people instead')
        .setStyle(ButtonStyle.Secondary),
    );
  } else if (mode === RECIPIENT_MODE_PICKER && voiceChannelId) {
    // Live count via interaction.guild for the label. Every production
    // caller (handleQurlSlashSend, handleConfirmUserSelect,
    // handleConfirmVoiceEveryone, rerenderConfirmCard) passes
    // interaction; renderer-shape unit tests that drive layout-only
    // assertions may omit it. Either degraded case (no interaction,
    // missing channel, no members collection) lands the button in
    // the disabled-with-count=null branch — `connectedCount == null`
    // is the single sentinel for "render the disabled button shell".
    //
    // Count is render-time, not click-time — members can join/leave
    // voice between renders. Click-time resolution in
    // handleConfirmVoiceEveryone is the authoritative recipient set;
    // the count here only drives the disable state.
    let connectedCount = null;
    if (interaction) {
      const channel = interaction.guild?.channels?.cache?.get?.(voiceChannelId);
      if (channel?.members) {
        let n = 0;
        for (const [, m] of channel.members) {
          if (!isBotMember(m)) n++;
        }
        connectedCount = n;
      }
    }
    bottomRow.addComponents(
      new ButtonBuilder()
        .setCustomId(CONFIRM_VOICE_EVERYONE_BUTTON_CUSTOM_ID)
        .setLabel('\u{1F50A} Everyone on voice')
        .setStyle(ButtonStyle.Secondary)
        .setDisabled(connectedCount == null || connectedCount === 0),
    );
  }
  // @everyone button — workaround for Discord's MentionableSelectMenu
  // filtering @everyone out of its dropdown. Gated on the sender's
  // MENTION_EVERYONE permission (channel-effective, same as the text-
  // path #323 gate) AND on picker-mode + guild context. In voice-mode
  // the card already targets the voice population, so showing the
  // button there would confuse "everyone" semantics. DM context has
  // no @everyone meaning.
  //
  // Count is render-time non-bot when the cache is fully populated
  // (`cache.size >= memberCount`); falls back to `guild.memberCount`
  // (total, includes bots — over-count, but the only reliable number
  // when the cache is cold). FILTER DRIFT (distinct from voice-
  // everyone's): voice-everyone's render-time count can drift from
  // click-time via members joining/leaving the voice channel between
  // render and click; @everyone's drift is cold-cache-overcounts-by-
  // bots OR stale-row-inflation (departed members lingering in cache
  // briefly between gateway events), resolved when the click-time
  // pre-warm fills the cache. In all cases the label is a hint and
  // `partitionRecipients` is the click-time authority.
  if (
    mode === RECIPIENT_MODE_PICKER
    && interaction && interaction.guild
    && interaction.memberPermissions?.has?.(PermissionFlagsBits.MentionEveryone) === true
  ) {
    const guild = interaction.guild;
    const { count: displayCount, accurate: countIsAccurate } = computeEveryoneDisplayCount(guild);
    // Disable on three conditions:
    //  - `null` (memberCount unavailable + cache cold) → `(?)` label
    //    so the user sees the button can't act vs. silent no-op
    //  - `0` reachable in degraded cache states (all-bots guild with
    //    sender's row temporarily missing). The bots-only click-time
    //    reject still surfaces the right copy, but disabling at render
    //    avoids the dead-click round trip
    //  - `> QURL_SEND_MAX_RECIPIENTS` ONLY when `countIsAccurate` —
    //    the cold-cache fallback returns `memberCount` (over-counts
    //    by bot population), so disabling on it would false-positive
    //    on a near-cap guild whose actual non-bot count is under cap
    //    (e.g., 20100 members with 200 bots → 19900 non-bots, well
    //    under default 20000 cap, but raw memberCount = 20100). Cold-
    //    cache over-cap defers to click-time hard-reject in
    //    handleConfirmEveryone (authoritative — partition runs against
    //    the prewarmed cache).
    const overCap = countIsAccurate && displayCount != null && displayCount > config.QURL_SEND_MAX_RECIPIENTS;
    // Fixed label — no live count, no overcap suffix. The disabled+
    // greyed-out button is the visual signal for the three degraded
    // states (count unavailable, empty guild, exceeds cap).
    //
    // UX TRADE-OFF: the warm-cache over-cap branch DOES disable here
    // with no in-card "exceeds N cap" hint anymore. Previously the
    // label spelled out the reason; now the user sees only a greyed-
    // out button. Deliberate per product call ("terse labels"); the
    // disable trigger is structurally rare (guild with > 20k non-bot
    // members), and the cold-cache over-cap branch still surfaces
    // the actionable copy via `handleConfirmEveryone`'s click-time
    // reject (this render-time disable can't fire on cold cache, so
    // the click goes through). If the warm-cache hint ever needs to
    // come back, the lever is right here.
    //
    // BOTTOM ROW COMPONENT BUDGET: Discord caps an ActionRow at 5
    // components. The current worst-case render is
    // [🔊 Voice] + [📢 @everyone] + Note + Send + Cancel = 5, exactly
    // at the limit. Adding another button below would silently throw
    // at discord.js builder time. Re-evaluate the layout (e.g., a
    // second row for affordances vs. fixed buttons) before adding.
    const everyoneDisabled = displayCount == null || displayCount === 0 || overCap;
    if (everyoneDisabled) {
      // Diagnostic: surface the exact state when the @everyone button
      // renders disabled. Users see only a greyed-out button with no
      // label hint, so reports like "why is @everyone disabled?"
      // require digging into renderConfirmCardRows state — capturing
      // it inline here makes those reports answerable from logs.
      //
      // Sampled at warn level (not info) because the disable is
      // expected to be RARE — every fire is a signal worth seeing.
      // If it turns out to be common in production we'll know to
      // demote the level. Branch label disambiguates which of the
      // three disable conditions fired without parsing the values.
      const branch = displayCount == null
        ? 'displayCount-null'
        : displayCount === 0
          ? 'displayCount-zero'
          : 'over-cap';
      logger.warn('@everyone button rendered disabled', {
        branch,
        guildId: interaction.guildId,
        guildMemberCount: interaction.guild?.memberCount ?? null,
        cacheSize: interaction.guild?.members?.cache?.size ?? null,
        displayCount,
        accurate: countIsAccurate,
        cap: config.QURL_SEND_MAX_RECIPIENTS,
      });
    }
    bottomRow.addComponents(
      new ButtonBuilder()
        .setCustomId(CONFIRM_EVERYONE_BUTTON_CUSTOM_ID)
        .setLabel('\u{1F4E2} @everyone')
        .setStyle(ButtonStyle.Secondary)
        .setDisabled(everyoneDisabled),
    );
  }
  bottomRow.addComponents(
    new ButtonBuilder()
      .setCustomId(CONFIRM_NOTE_BUTTON_CUSTOM_ID)
      .setLabel(personalMessage ? '✏\u{FE0F} Edit note' : '✏\u{FE0F} Add a note (optional)')
      .setStyle(ButtonStyle.Primary),
    new ButtonBuilder()
      .setCustomId(CONFIRM_SEND_CUSTOM_ID)
      .setLabel('\u{1F4E4} Send')
      .setStyle(ButtonStyle.Success)
      .setDisabled(sendDisabled),
    new ButtonBuilder()
      .setCustomId(CONFIRM_CANCEL_CUSTOM_ID)
      .setLabel('Cancel')
      .setStyle(ButtonStyle.Secondary),
  );
  rows.push(bottomRow);
  return rows;
}

// Shared entry point — both `/qurl send` and `/qurl map` route here
// after collecting their resource-specific options. `params` carries:
//   resourceType: RESOURCE_TYPES.FILE | RESOURCE_TYPES.MAPS
//   attachment:   {url, name, contentType, size} | null  (file path)
//   locationUrl:  string | null                          (map path)
//   locationName: string | null                          (map path)
//   resourceLabel: string                                (rendered on card)
// apiKey is INTENTIONALLY not threaded through the front-half.
// handleConfirmSendClick re-fetches the guild API key at Send time
// — the key may rotate during the confirm card's 3-min TTL, and the
// dispatcher's gate at the slash-command entry point only proves the
// key was present at that single moment. Re-fetching at click time
// is the durable check. The dispatcher's gate (API_KEY_GATED_SUBCOMMANDS
// set) still runs to fail fast on the no-key-at-all case.
async function handleQurlSlashSend(interaction, params) {
  // Guild check uses `interaction.guildId` only — that's the
  // authoritative "in a guild?" signal from the interaction payload.
  // `interaction.guild` is a derived property (`client.guilds.cache.get
  // (guildId)`) that returns null in http-only mode where there's no
  // gateway connection to populate the cache. Downstream `interaction
  // .guild.X` paths get the guild pre-fetched by the event-consumer
  // (see src/event-consumer.js pre-handle hook).
  if (!interaction.guildId) {
    return interaction.reply({
      content: 'This command can only be used in a server, not in DMs.',
      ephemeral: true,
    });
  }
  // Cooldown gate is owned by the front-half handlers (handleQurlSend /
  // handleQurlMap) so invalid inputs are throttled too. By the time we
  // get here the cooldown is already set; legitimate clearCooldown calls
  // on individual error branches below still unlock retry.

  // Top-level try/catch fences unanticipated throws (a TypeError from
  // a malformed cached member, a future parser change, etc.) from
  // leaving the user stranded in a cooldown window for a failure
  // that never produced a visible response. Every known failure
  // mode below has its own targeted clearCooldown + ephemeral
  // editReply; this catch is the safety net for everything else.
  //
  // Hoisted out of the try: `flow_id` (deterministic from the
  // interaction id) and `orphanFlowCreated` (flipped true ONLY after
  // supersedeOrCreate.created === true) so the catch can tell whether
  // a throw between supersedeOrCreate success and the final editReply
  // left an orphan DDB row that needs cleaning up.
  let flow_id;
  let orphanFlowCreated = false;
  try {
    // /qurl map defers before resolveLocation; skip the redundant
    // defer here so discord.js doesn't throw "Already replied or
    // deferred".
    if (!interaction.deferred) {
      await interaction.deferReply({ ephemeral: true });
    }

    const recipientsRaw = interaction.options.getString('recipients');
    const expiresIn = interaction.options.getString('expires-in') || '24h';
    const selfDestructValue = interaction.options.getString('self-destruct') || SELF_DESTRUCT_NO_TIMER_CHOICE;
    const personalMessageRaw = interaction.options.getString('personal-message');

    // Defense-in-depth: expiresIn comes from the slash command's choice
    // list which Discord enforces server-side, but a forged interaction
    // could carry an off-set value. EXPIRY_LABELS owns the closed set.
    if (!isValidExpiry(expiresIn)) {
      clearCooldown(interaction.user.id);
      return interaction.editReply({
        content: '❌ Unrecognized expiry value. Re-run and pick from the list.',
      });
    }
    const selfDestructSeconds = selfDestructOptionToSeconds(selfDestructValue);
    // Trim once and derive both forms — raw for Edit-note pre-fill,
    // sanitized for render. Without the raw, the modal would pre-fill
    // the escaped form and a no-op resubmit would double-escape.
    const initialRawTrimmed = personalMessageRaw
      ? safeCodepointSlice(personalMessageRaw.trim(), PERSONAL_MESSAGE_INPUT_MAX)
      : null;
    // `|| null`: sanitizeMessage returns '' (not null) when the input
    // strips to empty (ZWSP-only / bidi-only). Normalize to null so
    // downstream falsy checks AND the `personalMessage ? rawCapped : null`
    // lockstep below behave consistently.
    const personalMessage = initialRawTrimmed ? (sanitizeMessage(initialRawTrimmed) || null) : null;
    // Null out raw if sanitize stripped to empty (e.g. ZWSP-only input
    // survives `.trim()` but sanitizeMessage's bidi-strip removes it).
    // Without this, the Edit-note button would label as "Add a note"
    // (personalMessage is empty/falsy) but pre-fill with invisible chars.
    const personalMessageRawTrimmed = personalMessage ? initialRawTrimmed : null;

    // `@everyone` is gated on the sender's MENTION_EVERYONE permission
    // in this channel (Discord's own gate for mass-mention). Without
    // this, any guild member could blast a /qurl send to every member
    // in the cache, bounded only by the QURL_SEND_MAX_RECIPIENTS cap
    // (now 20k — sized for voice/stage-everyone, see config.js). The
    // parser surfaces `massMentionDenied: true` when the sender tried
    // but lacks permission — the caller renders a permission-specific
    // warning instead of the generic "couldn't parse" copy.
    //
    // `interaction.memberPermissions` is the resolved channel-effective
    // permission set (guild perms + channel overwrites). A future
    // refactor switching to `interaction.member.permissions` would
    // silently lose the channel-overwrite respect — keep this property.
    const canMentionEveryone = interaction.memberPermissions?.has(PermissionFlagsBits.MentionEveryone) === true;
    // Skip the fetch when only `@everyone` is in the input AND the
    // sender lacks MENTION_EVERYONE — the parser will deny the
    // expansion (massMentionDenied), so the chunk request would be
    // wasted. Role-mention shapes (`<@&id>`) still trigger the fetch
    // regardless of permission because role expansion gates on
    // `role.mentionable === true` per-role, not the global perm.
    const hasMassMention = MASS_MENTION_HINT_RE.test(recipientsRaw || '');
    const hasRoleMention = ROLE_MENTION_HINT_RE.test(recipientsRaw || '');
    if (hasMassMention && (canMentionEveryone || hasRoleMention)) {
      await prewarmGuildMembersCache(interaction.guild, { caller: 'handleQurlSlashSend' });
    }
    const parsed = parseRecipientMentions(recipientsRaw, interaction, {
      allowMassMention: canMentionEveryone,
    });

    let resolved = { users: [], unresolvedIds: [], transientFailureIds: [] };
    if (parsed.ids.length > 0) {
      try {
        resolved = await resolveRecipientUsers(interaction, parsed.ids);
      } catch (err) {
        clearCooldown(interaction.user.id);
        logger.error('qurl send/map: resolveRecipientUsers threw', {
          user_id: interaction.user.id, error: err && err.message,
        });
        return interaction.editReply({
          content: '❌ Could not resolve recipients. Please try again.',
        });
      }
    }

    const { valid, droppedBots, selfIncluded } = partitionRecipients(resolved.users, interaction.user.id);

    // `needsPicker` is true when the user supplied no `recipients:`
    // value at all. When they DID supply one but it post-filtered to
    // empty, hard-fail with a specific error so the user knows why
    // (vs. silently dropping into the picker, which would mask the
    // underlying mention-list problem).
    const recipientsOmitted = recipientsRaw == null || recipientsRaw.trim().length === 0;
    // Suppress the @everyone permission warning in DM context —
    // @everyone has no meaning in a DM (Discord doesn't expand it
    // there) and "requires the Mention Everyone permission" reads
    // strangely when there's no permission to grant. The other
    // warnings (bot/capped/unresolved) still apply and surface.
    //
    // `roleMentionsDenied` does NOT need an isDmContext guard: the
    // parser's role loop won't fire without a `guild`, so
    // `parsed.roleMentionsDenied` is always `[]` in DM —
    // `resolveRoleNames` returns `[]` for empty input. Symmetric
    // with the picker-path call site, which calls `resolveRoleNames`
    // unconditionally (the picker path can't reach DM context at
    // all because `canMentionEveryone` requires `interaction.guild`).
    const isDmContext = !interaction.guild;
    const roleMentionsDeniedNames = resolveRoleNames(interaction.guild, parsed.roleMentionsDenied);
    const warningsBlock = renderRecipientWarnings({
      invalidTokens: parsed.invalidTokens,
      cappedCount: parsed.cappedCount,
      unresolvedIds: resolved.unresolvedIds,
      transientFailureIds: resolved.transientFailureIds,
      droppedBots,
      massMentionDenied: parsed.massMentionDenied && !isDmContext,
      roleMentionsDeniedNames,
    });
    if (!recipientsOmitted && valid.length === 0) {
      clearCooldown(interaction.user.id);
      // Transient-only: every mentioned ID hit a 429 / gateway blip in
      // members.fetch. Generic "no valid recipients" misleads — the
      // user's mentions were valid, the lookup just failed. Encourage
      // retry instead of asking them to re-pick.
      const transientOnly = resolved.transientFailureIds.length > 0
        && resolved.unresolvedIds.length === 0
        && droppedBots === 0
        && parsed.invalidTokens.length === 0
        && parsed.cappedCount === 0
        && roleMentionsDeniedNames.length === 0;
      if (transientOnly) {
        return interaction.editReply({
          content: warningsBlock + '❌ **Could not look up recipients right now.** Try again in a moment.',
        });
      }
      // Parser silently strips bots from `<@id>` mentions when the
      // cache reports them (recipient-parser.js:~219) — those IDs
      // never reach `partitionRecipients`. If post-parse `ids` is
      // empty AND the user supplied non-empty `recipients`, surface
      // a "nothing usable" message even when partition's breakdown
      // is zero. This is what bot-only mention lists hit.
      const breakdownEmpty = droppedBots === 0
        && resolved.unresolvedIds.length === 0
        && resolved.transientFailureIds.length === 0
        && parsed.invalidTokens.length === 0
        && parsed.cappedCount === 0
        && roleMentionsDeniedNames.length === 0;
      const detail = breakdownEmpty
        ? '\n\nMake sure you @-mention real users (bots are skipped automatically).'
        : '';
      // Metric signal for the v1 cap-skew tracked in #304: a
      // mention-list that resolved to NOTHING (parser-stripped bots
      // before partition saw them) is exactly the failure mode the v2
      // resolve-then-cap refactor would fix. Log at INFO so #304's
      // prioritization has data without dialing verbosity up.
      if (breakdownEmpty && !recipientsOmitted) {
        logger.info('handleQurlSlashSend: bot-only-or-self mention list', {
          user_id: interaction.user.id, resource_type: params.resourceType,
        });
      }
      return interaction.editReply({
        content: warningsBlock + '❌ **No valid recipients to send to.** Re-run with at least one valid user mention.' + detail,
      });
    }

    // Voice-channel context snapshot. The "Everyone on voice"
    // confirm-card button is rendered when this is set.
    // Snapshot at slash-command time (NOT re-derived at button-click)
    // because:
    //   - interaction.channel at click time tracks the channel the
    //     ephemeral message lives in, which DOES match invocation
    //     channel in practice — but pinning via payload is the durable
    //     contract and matches the rest of the flow-state pattern.
    //   - A user who opens /qurl send from a voice channel, then
    //     drags themselves into a different channel mid-confirm,
    //     should still see "Everyone on voice" target
    //     the channel they invoked from, not the channel they're now
    //     in.
    // The actual member resolution still happens at click time via
    // channel.members — see handleConfirmVoiceEveryone.
    const voiceChannelId = isVoiceChannelType(interaction.channel?.type)
      ? interaction.channel.id
      : null;

    // Voice-everyone auto-default. When the slash command was invoked
    // from a voice channel AND `recipients:` was omitted, resolve to
    // voice-connected members (excluding sender + bots) and render the
    // card in voice-mode. This makes "/qurl send" from inside #voice
    // behave the way users naturally expect — the room is the audience.
    //
    // Sender is filtered pre-validity via partitionRecipients's
    // `excludeSender` option; the "Everyone on voice" affordance
    // semantically means "everyone else," not "and CC myself."
    //
    // Banner asymmetry on the picker-mode fallback: sender-only /
    // bots-only / truly-empty falls back QUIETLY (the user didn't ask
    // for voice-everyone — surfacing "voice is empty" would be noise),
    // while over-cap and cache-miss surface a banner because they're
    // degraded states the user can't otherwise diagnose. The picker +
    // bottom voice button remain available throughout so the user can
    // recover when voice state changes (someone joins, bots kicked).
    //
    // SNAPSHOT vs. CLICK-TIME asymmetry: this slash-entry path freezes
    // `recipientIds` at command receipt — someone joining the channel
    // between `/qurl send` and the Send click is NOT added. The "🔊
    // Everyone on voice" button (handleConfirmVoiceEveryone) goes the
    // other way: re-resolves `channel.members` at click time. The two
    // shapes are reachable from the same UI but produce different
    // recipient sets; that's a deliberate UX call (the auto-default
    // card shows "N users in #voice" and Send must mean that set,
    // not whoever happens to be in voice at click time). Documented
    // here so a future contributor doesn't "fix" the asymmetry.
    let recipientMode = RECIPIENT_MODE_PICKER;
    let finalValid = valid;
    let finalSelfIncluded = selfIncluded;
    let finalWarningsBlock = warningsBlock;
    // Text-mention `@everyone` (typed in the `recipients:` slash option,
    // or its `<@&{guildId}>` wire form) lands the card in EVERYONE
    // mode — same as clicking the 📢 @everyone confirm-card button.
    // Auto-filling the picker with up to 25 of the expanded set would
    // either misrepresent the recipient list (only a subset shows) or
    // invite a picker re-interaction that silently replaces the
    // expansion with a truncated subset via handleConfirmUserSelect.
    // Hiding the picker matches the button-click UX.
    //
    // MIXED MENTIONS: `recipients: @everyone @alice` lands in
    // EVERYONE mode too — alice is already in the @everyone set, so
    // the explicit mention is semantically absorbed (the parser
    // dedups via `seen`). A user trying to NARROW @everyone by
    // listing names doesn't get that — they get the full fan-out.
    // Correct behavior (you can't subtract from "everyone") but
    // worth knowing if support asks.
    //
    // Gated on `valid.length > 0` as defense-in-depth — pins the
    // EVERYONE-mode invariant locally so a future refactor that
    // loosens the upstream `!recipientsOmitted && valid.length === 0`
    // early-return can't silently land us in EVERYONE-mode with no
    // recipients (which would render a card with no recipients AND
    // no picker).
    //
    // ORDERING: this branch deliberately precedes the voice-mode
    // auto-default below. `massMentionExpanded` requires
    // `recipientsOmitted: false` (the user typed something to be
    // parsed), and voice-mode auto-default requires
    // `recipientsOmitted: true`, so the two branches are mutually
    // exclusive in production. The ordering still encodes "explicit
    // @everyone wins over voice auto-default" as documentation — a
    // future refactor that loosens either guard MUST keep this
    // precedence or the auto-default could shadow an explicit @-text.
    //
    // SENDER-PUSH DIVERGENCE: handleConfirmEveryone (the button-click
    // path) defensively pushes `interaction.user` when the sender's
    // row is missing from `members.cache` (shard-resume / partial-
    // chunk race after prewarm). The text path here intentionally
    // does NOT mirror that — `partitionRecipients` runs on the parser
    // output upstream, which has already been prewarmed at line 4142.
    // In the narrow race where the sender is missing post-prewarm,
    // the text path yields `selfIncluded: false` while a click on the
    // 📢 @everyone button would yield `selfIncluded: true`. Acceptable
    // because the user explicitly typed `@everyone` (so they're aware
    // of the recipient list at Send time), but documented so a future
    // contributor pursuing strict click/text parity knows the lever.
    if (parsed.massMentionExpanded && valid.length > 0) {
      recipientMode = RECIPIENT_MODE_EVERYONE;
      // `finalValid`, `finalSelfIncluded`, `finalWarningsBlock` stay as-is —
      // parser output is authoritative for EVERYONE mode (no voice-resolve
      // re-partition needed). Pinned here so a future refactor that adds
      // an EVERYONE-mode re-partition step doesn't forget which branch
      // owns the reassignment.
    } else if (recipientsOmitted && voiceChannelId) {
      // Read voice-connected members through the same channel cache
      // lookup that handleConfirmVoiceEveryone and the bottom-button
      // count use (`guild.channels.cache.get(voiceChannelId)`). Reading
      // via `interaction.channel.members` would land identical state in
      // production but diverge in tests and on the rare command-from-
      // -outside-guild path; the cache lookup is the established
      // contract for "voice members at this channel id."
      const voiceChannel = interaction.guild?.channels?.cache?.get?.(voiceChannelId);
      if (!voiceChannel?.members) {
        // Cache miss: voice channel evicted or `GuildVoiceStates`
        // intent dropped mid-flight. Surface a banner + log so a
        // degraded environment doesn't silently lose voice-mode.
        //
        // Banner replaces (not appends to) `warningsBlock`: the
        // text-path warnings can't exist on this branch because
        // `recipientsOmitted` is true (no `recipients:` arg means no
        // tokens to parse, so `warningsBlock` is always `''` here).
        logger.info('handleQurlSlashSend: voice-mode skipped — channel members cache missing', {
          user_id: interaction.user.id, voice_channel_id: voiceChannelId,
        });
        finalWarningsBlock = `⚠\u{FE0F} Couldn't read voice channel members — pick recipients below.\n\n`;
      } else {
        const voiceMembers = [];
        for (const [, m] of voiceChannel.members) {
          if (m?.user) voiceMembers.push(m.user);
        }
        const voicePart = partitionRecipients(
          voiceMembers, interaction.user.id, { excludeSender: true }
        );
        // Inverted shape: commit voice-mode iff the resolved set fits
        // the cap; otherwise branch by reason. Sender-only / truly-
        // empty falls through to picker-mode with NO banner (user
        // didn't ask for voice-everyone; there's nothing actionable to
        // surface). Bots-only DOES surface a banner — a voice channel
        // populated entirely by bots is the kind of "wait, didn't it
        // know I was in voice?" state that benefits from the dropped-
        // bots accounting being visible.
        if (voicePart.valid.length > 0
            && voicePart.valid.length <= config.QURL_SEND_MAX_RECIPIENTS) {
          recipientMode = RECIPIENT_MODE_VOICE;
          finalValid = voicePart.valid;
          finalSelfIncluded = false;
          // Voice path produces its own warnings — only droppedBots is
          // relevant when `recipients:` was omitted (no text-parsing
          // warnings apply). Surface bot drops so the user knows why the
          // count is lower than the channel's connected total.
          finalWarningsBlock = renderRecipientWarnings({
            droppedBots: voicePart.droppedBots,
          });
        } else if (voicePart.valid.length === 0 && voicePart.droppedBots > 0) {
          // Bots-only voice channel. Quiet fallback would leave the
          // user wondering why the auto-default didn't take; the bot-
          // drop accounting explains it.
          finalWarningsBlock = renderRecipientWarnings({
            droppedBots: voicePart.droppedBots,
          });
        } else if (voicePart.valid.length > config.QURL_SEND_MAX_RECIPIENTS) {
          // Over-cap (mirrors button-handler hard-reject). Under default
          // config (20k cap vs 99-member voice cap) unreachable, but a
          // shrunk env override could trip this — silent fallback would
          // leave the user wondering why voice-mode didn't take.
          //
          // Wording note: `voicePart.valid.length` is the POST-filter
          // count (sender + bots excluded). The Discord client's voice
          // panel shows raw connections, so phrasing this as "eligible
          // recipients" avoids a confusing cross-check (e.g., panel
          // shows 100 / banner says "39 connected"). The log field
          // stays `eligible` for consistency.
          logger.info('handleQurlSlashSend: voice-mode skipped — exceeds QURL_SEND_MAX_RECIPIENTS', {
            user_id: interaction.user.id,
            voice_channel_id: voiceChannelId,
            eligible: voicePart.valid.length,
            cap: config.QURL_SEND_MAX_RECIPIENTS,
          });
          // Banner replaces `warningsBlock` (always `''` on this
          // branch — see cache-miss comment above).
          finalWarningsBlock = `⚠\u{FE0F} Voice channel has ${voicePart.valid.length} eligible recipients `
            + `(max ${config.QURL_SEND_MAX_RECIPIENTS}) — pick recipients below.\n\n`;
        }
      }
    }
    const needsPicker = recipientMode === RECIPIENT_MODE_PICKER && recipientsOmitted;
    const recipientIds = finalValid.map((u) => u.id);

    // supersedeOrCreate handles the "another /qurl send/map is open"
    // case — sibling-flow disambig surfaces a stage-specific message;
    // same-stage rerun atomically claims the slot. Mirrors /qurl
    // revoke's pattern.
    flow_id = flowIdForInteraction(interaction);
    const sendNonce = crypto.randomBytes(8).toString('hex');
    // Persist resolved aliases keyed by id. `rerenderConfirmCard`
    // falls back to these when the menu/note interaction fires after
    // the member-cache entry has been evicted (rare given the 3-min
    // flow TTL, but the alternative is showing the raw snowflake).
    // resolveRecipientAlias returns the PLAIN form — markdown escape
    // happens at render time.
    const recipientAliases = Object.fromEntries(
      finalValid.map((u) => [u.id, resolveRecipientAlias(u, interaction)])
    );
    const payload = {
      resourceType: params.resourceType,
      attachment: params.attachment,
      locationUrl: params.locationUrl,
      locationName: params.locationName,
      resourceLabel: params.resourceLabel,
      recipientIds,
      recipientAliases,
      voiceChannelId,
      // Mode token. Drives both the row layout (picker vs. pick-people-
      // -instead button) and the "To:" copy. Defaults to 'picker' at
      // every read site so a row that pre-dates this field (in-flight
      // at deploy time) keeps the legacy shape until it TTLs out.
      recipientMode,
      expiresIn,
      selfDestructSeconds,
      personalMessage,
      // CONTRACT: `personalMessageRaw` is for modal-prefill ONLY.
      // It is the trimmed-and-capped user input WITHOUT sanitization
      // (no NFKC, no bidi/control strip, no @-mention defuse, no
      // markdown escape). Never read it from any rendering path or
      // downstream pipeline — only `personalMessage` (the sanitized
      // derivative) is safe for those. `handleConfirmSendClick`
      // explicitly picks `personalMessage` (not `...payload`) when
      // calling `executeSendPipeline`, which keeps this safe today;
      // a contract test pins that invariant.
      personalMessageRaw: personalMessageRawTrimmed,
      // One-time information surface — slash-entry warnings about
      // bot/unresolved mentions persist into the payload so menu
      // interactions (which don't recompute) still render them.
      // Picker re-pick OVERWRITES with a fresh warningsBlock from the
      // new partition. Voice-mode override replaces text-path warnings
      // with the voice-partition's droppedBots-only summary.
      warningsBlock: finalWarningsBlock,
      // Neutral notice signal — surfaced on the confirm card as
      // "Send includes you." when the sender is in the recipient
      // list. Persisted (rather than derived from
      // `recipientIds.includes(senderId)` at re-render) for the
      // same reason `warningsBlock` is persisted: keeps the menu
      // re-render path stateless about who computed what. Derivation
      // would also work — both shapes are defensible, but mirroring
      // warningsBlock keeps the payload contract uniform.
      // Always false in voice-mode (sender is excluded pre-validity).
      selfIncluded: finalSelfIncluded,
      sendNonce,
    };
    let supersede;
    try {
      supersede = await supersedeOrCreate({
        flow_id,
        stage: SEND_STAGE_AWAITING_CONFIRM,
        payload,
        ttl_seconds: SEND_FLOW_TTL_SECONDS,
      });
    } catch (err) {
      clearCooldown(interaction.user.id);
      logger.warn('handleQurlSlashSend: supersedeOrCreate threw', {
        flow_id, error: err && err.message,
      });
      return interaction.editReply({
        content: '❌ Could not start a send — please try again.',
      });
    }
    if (!supersede.created) {
      clearCooldown(interaction.user.id);
      const siblingMsg = siblingMessageForStage(supersede.surviving?.stage);
      return interaction.editReply({
        content: siblingMsg || '❌ Could not start a send — please try again.',
      });
    }
    // We own the row from here on — a throw before the final editReply
    // would orphan it. The catch below uses this flag to fire a
    // best-effort version-checked deleteFlow.
    orphanFlowCreated = true;

    const content = renderConfirmCardContent({
      resourceType: params.resourceType,
      resourceLabel: params.resourceLabel,
      validRecipients: finalValid,
      expiresIn,
      selfDestructSeconds,
      personalMessage,
      warningsBlock: finalWarningsBlock,
      needsPicker,
      interaction,
      selfIncluded: finalSelfIncluded,
      recipientMode,
      voiceChannelId,
    });
    const rows = renderConfirmCardRows({
      sendDisabled: needsPicker,  // Send stays disabled until UserSelectMenu fires
      expiresIn,
      selfDestructSeconds,
      personalMessage,
      voiceChannelId,
      interaction,
      recipientIds,
      recipientMode,
    });
    // `return await` (not bare `return`) is load-bearing: without it
    // a Discord-side rejection on the confirm-card delivery would
    // bypass the outer catch entirely, leaving the user with a set
    // cooldown AND an orphan flow row from the supersedeOrCreate
    // we just landed.
    return await interaction.editReply({ content, components: rows });
  } catch (err) {
    // Unanticipated throw. Always clear cooldown — the user got no
    // visible response, so they must not be locked out for the full
    // cooldown window. Try editReply first (interaction was deferred
    // up-front); if it also throws (the deferReply ITSELF failed, so
    // there's no deferred state to edit), fall back to reply() as
    // ephemeral. The double .catch keeps a failed fallback silent —
    // worst case the user sees nothing, but the cooldown is cleared
    // so they can retry.
    clearCooldown(interaction.user.id);
    logger.error('handleQurlSlashSend: unexpected throw', {
      user_id: interaction.user.id, flow_id, error: err && err.message, stack: err && err.stack,
    });
    // Best-effort cleanup of any orphan flow row we created. Only
    // fires when supersedeOrCreate succeeded and the throw happened
    // between then and the final editReply — otherwise the row
    // would sit in DDB until TTL eviction, blocking a fresh /qurl
    // file or /qurl map invocation under the sibling-flow guard.
    // Version-gated stage check keeps a concurrent confirm-click
    // from racing the delete and removing a row that's already
    // advanced past awaiting-confirm.
    if (orphanFlowCreated) {
      deleteFlow(flow_id, {
        stage: SEND_STAGE_AWAITING_CONFIRM,
        reason: 'terminal',
      }).catch((cleanupErr) => {
        logger.warn('handleQurlSlashSend: orphan-flow cleanup failed', {
          flow_id, error: cleanupErr && cleanupErr.message,
        });
      });
    }
    const errContent = '❌ Something went wrong — please try again.';
    return interaction.editReply({ content: errContent })
      .catch(() => interaction.reply({ content: errContent, ephemeral: true }).catch(logIgnoredDiscordErr));
  }
}

async function handleQurlSend(interaction) {
  // DM rejection first — no cooldown burned on a guild-only invocation
  // attempted from DMs.
  // Guild check uses `interaction.guildId` only — that's the
  // authoritative "in a guild?" signal from the interaction payload.
  // `interaction.guild` is a derived property (`client.guilds.cache.get
  // (guildId)`) that returns null in http-only mode where there's no
  // gateway connection to populate the cache. Downstream `interaction
  // .guild.X` paths get the guild pre-fetched by the event-consumer
  // (see src/event-consumer.js pre-handle hook).
  if (!interaction.guildId) {
    return interaction.reply({
      content: 'This command can only be used in a server, not in DMs.',
      ephemeral: true,
    });
  }

  // UX fast-fail BEFORE setCooldown — "bot too busy" is server-side
  // backpressure, not user fault; locking the user out for 30s on a
  // capacity rejection would be punitive.
  if (activeFileSends >= MAX_CONCURRENT_FILE_SENDS) {
    // Log at INFO so capacity-tuning has a trend signal — a sudden
    // burst of these would indicate either MAX_CONCURRENT_FILE_SENDS
    // is undersized or a downstream pipeline is stuck holding slots.
    logger.info('handleQurlSend: capacity backpressure', {
      user_id: interaction.user.id,
      active_file_sends: activeFileSends,
      max_concurrent: MAX_CONCURRENT_FILE_SENDS,
    });
    return interaction.reply({
      content: 'The bot is processing too many file sends right now. Please try again in a moment.',
      ephemeral: true,
    });
  }

  // Cooldown gate at entry — after capacity backpressure but BEFORE
  // any input-validation so invalid inputs (malformed attachment,
  // SSRF host, wrong type, oversize) still throttle. Matches the
  // pattern at commands.js:~1677.
  if (isOnCooldown(interaction.user.id)) {
    return interaction.reply({
      content: 'Please wait before sending again.',
      ephemeral: true,
    });
  }
  setCooldown(interaction.user.id);

  // Required-option lookups via `getAttachment(..., true)` /
  // `getString(..., true)` throw on a missing option. Discord enforces
  // required server-side, so production interactions can't trip
  // these — BUT the more likely cause of a hit in production is a
  // client/schema desync (gateway has the new command schema, bot
  // has the old, or vice versa) during a redeploy window, not abuse.
  // Clear the cooldown so the user can retry once the deploy
  // stabilizes. SSRF gate below still preserves cooldown (probing
  // the allow-list IS abuse-aligned and doesn't have a redeploy
  // explanation).
  let attachment;
  try {
    attachment = interaction.options.getAttachment('attachment', true);
  } catch (err) {
    logger.warn('handleQurlSend: required attachment option missing', {
      user_id: interaction.user.id, error: err && err.message,
    });
    clearCooldown(interaction.user.id);
    return interaction.reply({
      content: '❌ The `attachment:` option is required. Re-run with a file attached.',
      ephemeral: true,
    });
  }
  if (!attachment || typeof attachment.url !== 'string') {
    // Same client/schema-desync rationale — clear cooldown for retry.
    clearCooldown(interaction.user.id);
    return interaction.reply({
      content: '❌ Attachment is missing or malformed.',
      ephemeral: true,
    });
  }
  if (!isAllowedSourceUrl(attachment.url)) {
    logger.warn('handleQurlSend: attachment.url failed SSRF gate', {
      user_id: interaction.user.id, host: safeUrlHost(attachment.url),
    });
    return interaction.reply({
      content: '❌ Attachment source not allowed. Files must be uploaded via Discord, not linked from external URLs.',
      ephemeral: true,
    });
  }
  if (!isAllowedFileType(attachment.contentType)) {
    // Honest user error (picked the wrong file in the OS dialog); not
    // an abuse signal. Unlock so retry with a valid file is immediate.
    // SSRF rejection above stays throttled — probing the allow-list is
    // an abuse signal, type/size rejections are not.
    clearCooldown(interaction.user.id);
    return interaction.reply({
      content: `❌ File type not allowed: \`${escapeDiscordMarkdown(String(attachment.contentType || 'unknown'))}\`.`,
      ephemeral: true,
    });
  }
  if (attachment.size > MAX_FILE_SIZE) {
    // Same shape as the file-type rejection — honest user error.
    clearCooldown(interaction.user.id);
    return interaction.reply({
      content: `❌ File too large (${Math.round(attachment.size / 1024 / 1024)}MB). Maximum is ${Math.round(MAX_FILE_SIZE / 1024 / 1024)}MB.`,
      ephemeral: true,
    });
  }

  return handleQurlSlashSend(interaction, {
    resourceType: RESOURCE_TYPES.FILE,
    attachment: {
      url: attachment.url,
      name: attachment.name,
      contentType: attachment.contentType,
      size: attachment.size,
    },
    locationUrl: null,
    locationName: null,
    // sanitizeContentLabel: NFKC + strip bidi/zero-width/control +
    // codepoint-aware 256-cap + markdown-escape. The codepoint slice
    // prevents UTF-16 surrogate splits at the 256 boundary; the bidi
    // strip defends against U+202E spoofing in filenames (a crafted
    // upload could otherwise visually fake a different filename in
    // the confirm card).
    resourceLabel: sanitizeContentLabel(attachment.name),
  });
}

async function handleQurlMap(interaction) {
  // DM rejection first — no cooldown burned on a DM invocation.
  // Guild check uses `interaction.guildId` only — that's the
  // authoritative "in a guild?" signal from the interaction payload.
  // `interaction.guild` is a derived property (`client.guilds.cache.get
  // (guildId)`) that returns null in http-only mode where there's no
  // gateway connection to populate the cache. Downstream `interaction
  // .guild.X` paths get the guild pre-fetched by the event-consumer
  // (see src/event-consumer.js pre-handle hook).
  if (!interaction.guildId) {
    return interaction.reply({
      content: 'This command can only be used in a server, not in DMs.',
      ephemeral: true,
    });
  }
  // NOTE: No `activeFileSends` capacity gate here (unlike handleQurlSend).
  // The capacity gate guards the connector upload path that file sends
  // hit during back-half dispatch; map sends never touch that resource.
  // Sharing the gate would punish maps for file-pipeline saturation.
  //
  // Cooldown gate — cross-command bucket shared with /qurl send
  // (sendCooldowns Map). Tests pin the contract.
  if (isOnCooldown(interaction.user.id)) {
    return interaction.reply({
      content: 'Please wait before sending again.',
      ephemeral: true,
    });
  }
  setCooldown(interaction.user.id);

  // `getString('location', true)` throws on a missing option. Discord
  // enforces required server-side; the more likely cause in production
  // is a client/schema desync (redeploy timing) than abuse. Clear
  // cooldown on the throw catch so the user can retry once the deploy
  // stabilizes — same rationale as handleQurlSend's required-option
  // throw branch.
  let locationValue;
  try {
    // Defensive .slice mirrors the modal path's pattern at commands.js:~2096.
    // The slash option's setMaxLength(500) enforces this server-side, so
    // legitimate clients can't exceed it — the slice is forged-interaction
    // defense.
    //
    // Order: trim() FIRST so the 500-char slice measures CONTENT, not
    // whitespace. Slicing first would let a payload like
    // `<100 leading spaces> + <450 chars of content>` lose 50 content
    // chars to the cap; trimming first preserves the full content.
    locationValue = interaction.options.getString('location', true).trim().slice(0, 500);
  } catch (err) {
    logger.warn('handleQurlMap: required location option missing', {
      user_id: interaction.user.id, error: err && err.message,
    });
    clearCooldown(interaction.user.id);
    return interaction.reply({
      content: '❌ The `location:` option is required. Re-run with a Google Maps URL or address.',
      ephemeral: true,
    });
  }
  const locationNameRaw = interaction.options.getString('location-name');
  if (locationValue.length === 0) {
    // Honest user error (pasted only whitespace, or empty string) —
    // unlock retry. Same shape as the file-type / size cap branches
    // in handleQurlSend.
    clearCooldown(interaction.user.id);
    return interaction.reply({
      content: '❌ Location is empty.',
      ephemeral: true,
    });
  }

  // Shared parser: see `parseLocationInput` near the top of this file.
  // Wrap in try/catch as a defensive symmetry with
  // handleQurlSend's catches: a future regex change in parseLocationInput
  // or a pathological input could throw synchronously before reaching
  // handleQurlSlashSend's safety net, leaving cooldown set with no
  // visible response.
  let parsedLocation;
  try {
    parsedLocation = parseLocationInput(locationValue);
  } catch (err) {
    logger.error('handleQurlMap: parseLocationInput threw', {
      user_id: interaction.user.id, error: err && err.message,
    });
    clearCooldown(interaction.user.id);
    return interaction.reply({
      content: '❌ Could not parse location — please re-run with a Google Maps URL or address.',
      ephemeral: true,
    });
  }

  // Defer before Places resolve to stay inside Discord's 3 s ACK window.
  // handleQurlSlashSend below checks `interaction.deferred` and skips
  // its own defer.
  try {
    await interaction.deferReply({ ephemeral: true });
  } catch (err) {
    // Token already expired or Discord transiently degraded — clear
    // cooldown so the user can retry without waiting it out.
    logger.warn('handleQurlMap: deferReply failed', {
      user_id: interaction.user.id, error: err && err.message,
    });
    clearCooldown(interaction.user.id);
    return undefined;
  }
  const resolved = await resolveLocation(parsedLocation);
  if (!resolved.ok) {
    clearCooldown(interaction.user.id);
    let content;
    switch (resolved.reason) {
      case RESOLVE_REASON.NO_API_KEY:
        content = '❌ Location search is unavailable on this server. Re-run with a full Google Maps URL.';
        break;
      case RESOLVE_REASON.NOT_FOUND:
        // A stale-sentinel miss (sender picked a suggestion that's since
        // been deleted upstream) gets a place-specific message. For
        // free-text misses, echo the (sanitized) input so the sender
        // can see what they typed.
        content = parsedLocation.placeId
          ? '❌ That place is no longer available. Pick another suggestion or paste a Google Maps URL.'
          : `❌ Couldn't find a place matching "${sanitizeContentLabel(locationValue.slice(0, 80))}". Try a more specific name or paste a Google Maps URL.`;
        break;
      default:
        content = '❌ Location lookup failed. Please try again, or paste a Google Maps URL.';
    }
    return interaction.editReply({ content });
  }
  let { locationUrl, locationName } = resolved;

  // Explicit location-name override wins over the resolved name
  // (whether that came from a URL, a place_id lookup, or a free-text
  // resolve).
  if (locationNameRaw && locationNameRaw.trim().length > 0) {
    locationName = locationNameRaw.trim();
  }
  // sanitizeContentLabel: NFKC + strip bidi/zero-width/control +
  // codepoint-aware 256-cap + markdown-escape. The bidi strip is
  // load-bearing here — /qurl map's slash option is a new
  // attack surface (no modal friction) and a crafted U+202E in
  // `location-name` would otherwise let the sender visually spoof
  // a Maps URL inside the rendered confirm card. Also handles the
  // UTF-16 surrogate-split risk at the 256 boundary.
  if (locationName) locationName = sanitizeContentLabel(locationName);

  return handleQurlSlashSend(interaction, {
    resourceType: RESOURCE_TYPES.MAPS,
    attachment: null,
    locationUrl,
    locationName,
    resourceLabel: locationName || 'location',
  });
}

// ──────────────────────────────────────────────────────────────────────
// /qurl detect — watermark attribution (#1101)
// ──────────────────────────────────────────────────────────────────────

// Shared no-match reply. BOTH the "no mark at all" branch and the
// "mark found but no same-guild row" branch (the cross-guild defense-in-depth
// filter) MUST emit BYTE-IDENTICAL copy. If they differed, a requester could
// distinguish "no watermark" from "watermarked, but for another guild" — and
// that distinction IS the cross-guild signal the second-layer filter exists
// to contain (it only matters if the connector's X-Guild-Id scope regresses,
// which is exactly when this filter is load-bearing). One constant = the two
// paths can't drift apart. (The ambiguous >1-row reply is deliberately
// distinct — that's a same-guild duplicate, not a cross-guild leak.)
const DETECT_NO_MATCH_MSG = '🔍 No qURL watermark found in this image (for this server).';

// Server-staff permissions that grant /qurl detect standing — the PM "admins/mods" tier, layered
// on top of the original-sender tier (see the handler's ACCESS MODEL). Deliberately TIGHT and
// governance-oriented, not a broad enumeration of every mod bit:
//   - ManageGuild — this bot's own admin gate (matches /qurl status + setup); an Administrator
//     passes it automatically via PermissionsBitField.has().
//   - BanMembers — the moderator tier that handles the PM's governance examples (abuse reports,
//     DMCA, ban appeals).
// Excludes broader content perms (e.g. ManageMessages) on purpose: ship tight, loosen if real
// demand surfaces (a one-line change). A caller with NONE of these who is also not the original
// sender of the qURL has no standing and gets the byte-identical no-match reply.
const DETECT_STAFF_PERMISSIONS = [
  PermissionFlagsBits.ManageGuild,
  PermissionFlagsBits.BanMembers,
];

//
// A user uploads an image; the bot reads the invisible meta-seal watermark
// via the connector's /api/detect, resolves it to the qurl_id it was minted
// for, and replies (EPHEMERALLY — it's sensitive) with the original
// RECIPIENT's Discord handle + a % match. SCOPED to the requester's guild:
// an image watermarked in guild A must never attribute in guild B.
//
// ACCESS MODEL (PM decision, #1101 — "privacy is the north star"): detect REVEALS a recipient
// only to someone with STANDING to investigate —
//   (a) the ORIGINAL SENDER of that qURL (their distribution, their property right), or
//   (b) server STAFF (ManageGuild / BanMembers — see DETECT_STAFF_PERMISSIONS) for governance
//       (abuse reports, DMCA, ban appeals, where the sender may be absent or uninvolved).
// A general member has NO standing and gets the BYTE-IDENTICAL no-match reply — they learn
// nothing, not even that the image is a watermarked qURL (a partial result + "ask an admin" would
// just train users to route around the privacy model). The standing check runs BEFORE the
// ambiguity/reveal branches so EVERY no-standing outcome collapses to ONE reply. The permission
// set is tight by design and a one-line, reversible change if real demand surfaces.
//
// RESIDUAL (timing side-channel): the byte-identical reply closes the CONTENT channel but not a
// TIMING one — no-mark returns BEFORE the DDB lookup, no-standing AFTER it, and a matched reveal
// after an extra users.fetch, so reply latency weakly clusters fast/medium/slow. A determined
// prober could use that to distinguish "no mark" from "marked, no standing". Accepted as a residual,
// NOT equalized with constant-time work: it's a weak oracle (ephemeral replies, network jitter, the
// per-(guild,user) cooldown + the connector's per-guild throttle all blunt it) and the connector's
// guild scope stays the authoritative boundary regardless.
//
// SCOPE (threat model): the standing gate is a bot-side UX/governance layer, NOT a server-enforced
// authorization boundary. The connector's /api/detect enforces only guild-scope; a holder of the
// guild's qURL API key (admin-tier) could call /api/detect directly and bypass THIS gate (guild-
// scope still holds — never cross-tenant). Closing that gap needs a connector-side per-route auth
// factor (tracked in qurl-integrations-infra#1170); until then, standing is enforced here.
//
// This is a deanonymization oracle by construction. The guards, in order:
//   - guild-only (DM rejects, no oracle outside a guild).
//   - per-(guild,user) cooldown (detectCooldowns) — best-effort
//     defense-in-depth against scraping the recipient↔image mapping. The
//     AUTHORITATIVE rate limit is the connector's per-guild throttle (this
//     in-process Map isn't shared across instances); see the detectCooldowns
//     header for the full enforcement model.
//   - SSRF + size + image-type validation on the attachment, identical to
//     the guards handleQurlSend applies to the same Discord-CDN fetch.
//   - TWO layers of guild scoping: the connector scopes via X-Guild-Id,
//     AND we re-filter the returned rows on guild_id here (defense-in-depth
//     — if the connector contract ever regresses, the leak still can't
//     cross guilds because this filter fails closed).
//   - STANDING gate (the ACCESS MODEL above): reveal only to the original
//     sender or to staff; no-standing callers get the byte-identical no-match
//     reply, checked BEFORE the ambiguity/reveal branches.
//   - audit the attribution outcomes + abuse signals (matched / no_match /
//     ambiguous / rejected / unconfigured / rate_limited / no_standing — see
//     AUDIT_EVENTS.QURL_DETECT); recipient id NEVER logged (audit access is
//     broader than the ephemeral reply). Honest operational failures (CDN
//     download, connector 5xx/network) log at warn/error, not audit.
//
// Reply is always ephemeral. Cooldown rationale per-branch mirrors
// handleQurlSend: honest user errors (missing/non-image/oversize attachment,
// client-schema desync) CLEAR the cooldown so retry is immediate; the SSRF
// probe KEEPS it (probing the allow-list is an abuse signal).
async function handleQurlDetect(interaction) {
  // DM rejection first — no cooldown burned on a guild-only command
  // attempted from DMs. Same `interaction.guildId`-only signal as
  // handleQurlSend (see its comment for the http-only-mode caveat).
  if (!interaction.guildId) {
    return interaction.reply({
      content: 'This command can only be used in a server, not in DMs.',
      ephemeral: true,
    });
  }

  // Cooldown gate at entry — per (guild, user). Set BEFORE input
  // validation so a malformed attachment still throttles (so spamming
  // invalid inputs can't sidestep the bot-side throttle; the connector's
  // per-guild rate limit is the authoritative backstop).
  if (isOnDetectCooldown(interaction.guildId, interaction.user.id)) {
    return interaction.reply({
      content: 'Please wait before running detect again.',
      ephemeral: true,
    });
  }
  setDetectCooldown(interaction.guildId, interaction.user.id);

  // Required-option lookup. `getAttachment(name, true)` throws on a
  // missing option; Discord enforces required server-side, so a hit is
  // almost always a client/schema desync during a redeploy window — clear
  // the cooldown so retry is immediate once the deploy stabilizes (same
  // rationale as handleQurlSend's required-option catch).
  let attachment;
  try {
    attachment = interaction.options.getAttachment('image', true);
  } catch (err) {
    logger.warn('handleQurlDetect: required image option missing', {
      user_id: interaction.user.id, error: err && err.message,
    });
    clearDetectCooldown(interaction.guildId, interaction.user.id);
    return interaction.reply({
      content: '❌ The `image:` option is required. Re-run with an image attached.',
      ephemeral: true,
    });
  }
  if (!attachment || typeof attachment.url !== 'string') {
    clearDetectCooldown(interaction.guildId, interaction.user.id);
    return interaction.reply({
      content: '❌ Attachment is missing or malformed.',
      ephemeral: true,
    });
  }
  // SSRF gate — the attachment.url is fetched below; re-validate it
  // against the Discord-CDN allow-list exactly like handleQurlSend. This
  // is the ONE rejection that KEEPS the cooldown — probing the allow-list
  // is an abuse signal, not an honest user error.
  if (!isAllowedSourceUrl(attachment.url)) {
    logger.warn('handleQurlDetect: attachment.url failed SSRF gate', {
      user_id: interaction.user.id, host: safeUrlHost(attachment.url),
    });
    // Audit the SSRF probe — it's the STRONGEST abuse signal this handler
    // sees (it's the one rejection that KEEPS the cooldown), so it belongs
    // in the audit trail, not just logger.warn. No recipient id: a
    // rejection never resolves one.
    logger.audit(AUDIT_EVENTS.QURL_DETECT, {
      result: 'rejected',
      guild_id: interaction.guildId,
      requester_id: interaction.user.id,
    });
    return interaction.reply({
      content: '❌ Attachment source not allowed. Upload the image via Discord, not a linked URL.',
      ephemeral: true,
    });
  }
  // Image-only: the connector's neural detector reads pixels. A non-image
  // (PDF, zip, etc.) can't carry the meta-seal mark, so reject early
  // rather than spend an inference round-trip. Honest user error → clear
  // cooldown.
  if (typeof attachment.contentType !== 'string' || !attachment.contentType.startsWith('image/')) {
    clearDetectCooldown(interaction.guildId, interaction.user.id);
    return interaction.reply({
      content: `❌ Detect needs an image. \`${escapeDiscordMarkdown(String(attachment.contentType || 'unknown'))}\` isn't supported.`,
      ephemeral: true,
    });
  }
  // Size cap before buffering — guard against pulling a 25MB+ blob into
  // memory. Honest user error → clear cooldown. `typeof === 'number'`
  // guard: a missing/undefined attachment.size would make a bare
  // `size > MAX` read false and skip this pre-check — harmless here only
  // because the realized-buffer cap after the fetch (below) is the real
  // backstop, but make the intent explicit rather than relying on
  // `undefined > N` semantics.
  if (typeof attachment.size === 'number' && attachment.size > MAX_FILE_SIZE) {
    clearDetectCooldown(interaction.guildId, interaction.user.id);
    return interaction.reply({
      content: `❌ Image too large (${Math.round(attachment.size / 1024 / 1024)}MB). Maximum is ${Math.round(MAX_FILE_SIZE / 1024 / 1024)}MB.`,
      ephemeral: true,
    });
  }

  // Defer EARLY (ephemeral) — everything past here is network-bound (CDN
  // fetch + neural-net inference ~seconds + a recipient users.fetch), which
  // would blow Discord's 3-second ACK deadline on a plain reply. After
  // deferReply, all user-visible output is editReply.
  await interaction.deferReply({ ephemeral: true });

  // Resolve the API key the same way the send paths do: per-guild BYOK
  // first, global fallback. (handleQurlDetect resolves its own key — it's
  // intentionally NOT in API_KEY_GATED_SUBCOMMANDS, which would
  // double-resolve and gate before this handler runs.)
  const apiKey = await db.getGuildApiKey(interaction.guildId) || config.QURL_API_KEY;
  if (!apiKey) {
    // A missing /qurl setup is an honest config error, not abuse — clear
    // the cooldown so the user can retry the instant an admin configures
    // the server. Matches the handler's "honest user errors clear the
    // cooldown" design (non-image / oversize branches above). The SSRF
    // probe is the one rejection that intentionally KEEPS the cooldown.
    clearDetectCooldown(interaction.guildId, interaction.user.id);
    // Audit this branch — unconfigured is an attribution outcome worth
    // surfacing (see the handler header's audit list). No recipient is
    // resolved on an unconfigured guild.
    logger.audit(AUDIT_EVENTS.QURL_DETECT, {
      result: 'unconfigured',
      guild_id: interaction.guildId,
      requester_id: interaction.user.id,
    });
    return interaction.editReply({
      content: '❌ **qURL is not configured for this server.** A server admin needs to run `/qurl setup` first.',
    });
  }

  // Download the image bytes from Discord CDN. This is a SEPARATE failure
  // domain from the connector detect below — a download failure (expired
  // signed URL, timeout, DNS, network) is honest + re-uploadable and must
  // NOT be reported as "detection unavailable" (detection was never
  // reached). Its own try/catch keeps the two error surfaces distinct, and
  // both the non-ok and the throw paths CLEAR the cooldown (honest, and the
  // reply tells the user to re-upload, which the cooldown would block).
  let bytes;
  try {
    const r = await fetch(attachment.url, { signal: AbortSignal.timeout(30000) });
    if (!r.ok) {
      // CDN URLs are signed + expire (~24h). A non-ok fetch is almost always
      // an expired URL on a re-shared old image, not our bug.
      logger.warn('handleQurlDetect: CDN fetch failed', {
        user_id: interaction.user.id, status: r.status,
      });
      clearDetectCooldown(interaction.guildId, interaction.user.id);
      return interaction.editReply({
        content: '❌ Could not download that image (the Discord link may have expired). Re-upload and try again.',
      });
    }
    bytes = Buffer.from(await r.arrayBuffer());
  } catch (err) {
    // Fetch threw — AbortSignal 30s timeout, DNS, connection reset, etc.
    // Honest download failure (NOT a detection failure), so same copy +
    // cooldown-clear as the non-ok branch above.
    logger.warn('handleQurlDetect: CDN fetch threw', {
      user_id: interaction.user.id, error: err && err.message,
    });
    clearDetectCooldown(interaction.guildId, interaction.user.id);
    return interaction.editReply({
      content: '❌ Could not download that image (re-upload and try again).',
    });
  }

  // Realized-buffer size guard (closes the TOCTOU vs. the attachment.size
  // pre-check): re-check the ACTUAL downloaded byte count. SSRF already
  // pins the source to the Discord CDN, so this is defense-in-depth against
  // a lying/absent Content-Length or a metadata/payload mismatch. Honest
  // oversize → clear cooldown, same as the pre-fetch size branch.
  if (bytes.length > MAX_FILE_SIZE) {
    logger.warn('handleQurlDetect: realized buffer exceeds cap', {
      user_id: interaction.user.id, bytes: bytes.length,
    });
    clearDetectCooldown(interaction.guildId, interaction.user.id);
    return interaction.editReply({
      content: `❌ Image too large (${Math.round(bytes.length / 1024 / 1024)}MB). Maximum is ${Math.round(MAX_FILE_SIZE / 1024 / 1024)}MB.`,
    });
  }

  // POST the bytes to /api/detect.
  let result;
  try {
    result = await detectWatermark(bytes, {
      guildId: interaction.guildId,
      contentType: attachment.contentType,
      apiKey,
    });
  } catch (err) {
    // detectWatermark (the CONNECTOR POST — the CDN download is handled in
    // its own try/catch above) throws (with .status, set from
    // response.status in connector.js's throwConnectorError) on
    // 401/400/429/5xx and on a connector-side network failure. Body-free
    // message by contract — log server-side, surface a generic ephemeral
    // error. Don't leak err.message (it may echo connector detail) into the
    // reply.
    //
    // Cooldown policy: a transient failure shouldn't throttle an honest
    // user for the full window, so CLEAR the cooldown — EXCEPT on a 429
    // (connector rate-limited this guild), where KEEPING the cooldown is
    // the correct back-off. (429 is cleanly distinguishable via err.status;
    // connector network throws / 5xx / 4xx-other all clear.)
    const rateLimited = err && err.status === 429;
    logger.error('handleQurlDetect: detect failed', {
      user_id: interaction.user.id,
      guild_id: interaction.guildId,
      status: err && err.status,
      rate_limited: rateLimited,
      error: err && err.message,
    });
    if (rateLimited) {
      // A connector 429 means this guild is spamming detect into the
      // connector's per-guild limiter — an abuse signal, and the one detect
      // failure that KEEPS the cooldown. Mirror the SSRF-probe reasoning and
      // put it in the audit trail (not just logger.error) so the
      // throttled-spam pattern is queryable. No recipient id — a throttled
      // call never resolves one.
      logger.audit(AUDIT_EVENTS.QURL_DETECT, {
        result: 'rate_limited',
        guild_id: interaction.guildId,
        requester_id: interaction.user.id,
      });
    } else {
      clearDetectCooldown(interaction.guildId, interaction.user.id);
    }
    return interaction.editReply({
      content: rateLimited
        ? '❌ Watermark detection is busy right now (rate-limited). Please wait a moment and try again.'
        : '❌ Watermark detection is unavailable right now. Please try again in a moment.',
    });
  }

  // No mark, OR the connector found a mark but no SAME-GUILD match
  // (detected=false collapses both — qurl_id/match_pct null). Audit the
  // no-match outcome, then a non-committal ephemeral (don't reveal whether
  // a mark exists in some OTHER guild — that itself would leak cross-guild
  // signal).
  if (!result.detected || !result.qurl_id) {
    logger.audit(AUDIT_EVENTS.QURL_DETECT, {
      result: 'no_match',
      guild_id: interaction.guildId,
      requester_id: interaction.user.id,
    });
    return interaction.editReply({
      // Shared constant — MUST match the cross-guild-filtered branch below
      // byte-for-byte. See DETECT_NO_MATCH_MSG.
      content: DETECT_NO_MATCH_MSG,
    });
  }

  // Connector says detected + gave a qurl_id. Look up the recipient
  // row(s). DEFENSE-IN-DEPTH guild filter: the connector already
  // guild-scopes via X-Guild-Id, but re-filter on guild_id here so a
  // connector-side contract regression can't cross guilds — this filter
  // fails closed (a row missing guild_id, or carrying a different one, is
  // dropped). findSendsByQurlId returns 0–1 rows in steady state
  // (one qurl_id per recipient by write-path invariant, capped at Limit:2).
  const sends = await db.findSendsByQurlId(result.qurl_id);
  const sameGuild = (sends || []).filter((s) => s.guild_id === interaction.guildId);
  if (sameGuild.length === 0) {
    // Connector matched but no local same-guild row — treat as no match
    // (the local row may have aged out, or the connector's scope and ours
    // diverged). BYTE-IDENTICAL copy to the no-mark branch above (shared
    // DETECT_NO_MATCH_MSG) so a requester can't tell "no watermark" from
    // "watermarked for another guild" — that distinction is the cross-guild
    // signal this filter exists to contain.
    logger.audit(AUDIT_EVENTS.QURL_DETECT, {
      result: 'no_match',
      guild_id: interaction.guildId,
      requester_id: interaction.user.id,
      qurl_id: result.qurl_id,
    });
    return interaction.editReply({
      content: DETECT_NO_MATCH_MSG,
    });
  }

  // STANDING gate (ACCESS MODEL): reveal only to the original SENDER of this qURL or to server
  // STAFF. Runs BEFORE the ambiguity/reveal branches ON PURPOSE — every no-standing outcome (no
  // mark, marked-for-another-guild, ambiguous, no-standing) must collapse to the SAME
  // byte-identical reply, or a prober could distinguish "this is a marked qURL" from "no mark".
  // Standing is checked with .some() over the rows so the sender check still holds in the
  // degenerate length-2 (ambiguous) case.
  const isStaff = DETECT_STAFF_PERMISSIONS.some(
    (p) => interaction.memberPermissions?.has(p) === true,
  );
  const isSender = sameGuild.some((s) => s.sender_discord_id === interaction.user.id);
  if (!isStaff && !isSender) {
    // No standing → audit the truth (operators see it) but reply byte-identical to no-match (the
    // caller learns nothing). KEEP the cooldown — a no-standing probe is sensitive, not an honest
    // input error, so it must not reset the throttle.
    logger.audit(AUDIT_EVENTS.QURL_DETECT, {
      result: 'no_standing',
      guild_id: interaction.guildId,
      requester_id: interaction.user.id,
      qurl_id: result.qurl_id,
    });
    return interaction.editReply({
      content: DETECT_NO_MATCH_MSG,
    });
  }

  if (sameGuild.length > 1) {
    // Ambiguous: >1 same-guild row for one qurl_id violates the
    // one-qurl_id-per-recipient write invariant (a write-path regression
    // landed a duplicate). Mirror the qurl.expired handler's ambiguous-skip
    // — do NOT attribute to one row arbitrarily (it could finger the wrong
    // recipient). Surface a non-committal reply + audit so an operator can
    // investigate. findSendsByQurlId's Limit:2 means length is exactly 2
    // here; the audit's qurl_id is the investigation handle.
    logger.audit(AUDIT_EVENTS.QURL_DETECT, {
      result: 'ambiguous',
      guild_id: interaction.guildId,
      requester_id: interaction.user.id,
      qurl_id: result.qurl_id,
    });
    return interaction.editReply({
      content: '🔍 Couldn\'t determine a single recipient for this image. Please contact a server admin.',
    });
  }

  const send = sameGuild[0];
  // How this caller earned standing — 'sender' (their own qURL) or 'staff' (governance). Recorded
  // in the match audit for forensics; the user-facing reveal is identical either way.
  const grantBasis = isSender ? 'sender' : 'staff';
  const matchPct = typeof result.match_pct === 'number' ? Math.round(result.match_pct) : null;

  // Audit the MATCH — qurl_id + match_pct + confidence, but NEVER the
  // resolved recipient id. Audit logs are broader-access than the ephemeral
  // reply; logging the unmasked recipient would re-leak the deanonymization
  // the ephemeral is there to contain. confidence (0–1) lives HERE, not in
  // the user reply — see the reply below. match_pct is logged RAW
  // (result.match_pct, not the display-rounded matchPct) so forensics keeps
  // the connector's exact value, not the value the user happened to see.
  logger.audit(AUDIT_EVENTS.QURL_DETECT, {
    result: 'matched',
    guild_id: interaction.guildId,
    requester_id: interaction.user.id,
    qurl_id: result.qurl_id,
    match_pct: result.match_pct,
    confidence: result.confidence,
    grant_basis: grantBasis,
  });

  // Resolve the recipient's handle. Best-effort: if the user can't be
  // fetched (left Discord, API blip), still surface the attribution with
  // the raw mention (Discord renders <@id> client-side) rather than fail
  // the whole detect.
  let username = null;
  try {
    const user = await interaction.client.users.fetch(send.recipient_discord_id);
    username = user && user.username;
  } catch (err) {
    logger.warn('handleQurlDetect: recipient user fetch failed', {
      user_id: interaction.user.id, error: err && err.message,
    });
  }

  const id = send.recipient_discord_id;
  const who = username ? `**${escapeDiscordMarkdown(username)}** (<@${id}>)` : `<@${id}>`;
  // match_pct (0–100) is the headline the user asked for. confidence (0–1)
  // is deliberately OMITTED from the reply — mixing a 0–100 and a 0–1 scale
  // confused readers, and an absent confidence rendered a bare "0". It stays
  // in the match audit above for operators.
  //
  // Suppress the "% match" suffix unless match_pct is a POSITIVE number:
  // a detected match always has match_pct > 0, so a null/0/garbled value
  // is a malformed connector response, not a real 0% — render the bare
  // attribution rather than a confusing "0% match". (The audit above keeps
  // the raw value for forensics.) The > 0 test is on the ROUNDED value, which
  // is safe because the connector FLOORS a detected match_pct at 75 (Hamming
  // path, distance <= max) or 100 (exact hit) — so a value that rounds to 0 is
  // genuinely malformed, never a real sub-1% match being dropped.
  const pctText = (typeof matchPct === 'number' && matchPct > 0) ? ` — ${matchPct}% match` : '';
  return interaction.editReply({
    content: `🔍 This image was watermarked for ${who}${pctText}.`,
    // The reply embeds the recipient's <@id> mention. Ephemeral replies
    // don't ping in practice, but suppress mention-resolution explicitly so
    // "the deanonymized recipient is NEVER pinged" is a hard guarantee, not
    // a reliance on Discord ephemeral behavior. parse:[] = resolve nothing.
    allowedMentions: { parse: [] },
  });
}

// --- Confirm-card handlers for `/qurl send` + `/qurl map` ---
// Any future rename of the `qurl_confirm_*` wire literals (or these
// handler names, since they're paired with them via registerFlow)
// needs a SEND_FLOW_TTL_SECONDS (180s) drain on the prior deploy so
// in-flight `flow_state` rows don't orphan across the boundary.
//
// Stage stays at SEND_STAGE_AWAITING_CONFIRM — `transitionFlow` with
// `stage_to` === current stage advances the version (OCC guard) and
// refreshes the TTL, so repeated picker churn doesn't expire the row
// while the user is still deciding.
async function handleConfirmUserSelect(interaction, { flow_id, row }) {
  // `deferUpdate` first so the downstream `transitionFlow` (DDB OCC
  // update) can take more than Discord's 3-second hard ack deadline
  // without surfacing as an "interaction failed" toast. Mirrors
  // handleConfirmSendClick / handleConfirmCancelClick. All `update`
  // calls below become `editReply` (the interaction is now deferred).
  await interaction.deferUpdate().catch(logIgnoredDiscordErr);

  // Validate payload.resourceType BEFORE renderConfirmCardContent
  // would throw on it. A corrupt/stale DDB row (manual mutation,
  // schema drift, etc.) with an unknown resourceType would otherwise
  // throw TypeError from the renderer and surface the dispatcher's
  // generic "superseded" copy — wrong, since nothing was superseded.
  // Delete the corrupt row + surface actionable "re-run" copy.
  const payloadResource = (row.payload || {}).resourceType;
  if (payloadResource !== RESOURCE_TYPES.FILE && payloadResource !== RESOURCE_TYPES.MAPS) {
    logger.error('handleConfirmUserSelect: corrupt flow payload (unknown resourceType)', {
      flow_id, resource_type: payloadResource,
    });
    await deleteFlow(flow_id, {
      stage: SEND_STAGE_AWAITING_CONFIRM,
      reason: 'terminal',
    }).catch(logIgnoredDiscordErr);
    return interaction.editReply({
      content: '❌ Card data is corrupted — please re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }

  // Resolve the MentionableSelectMenu pick: merges picked users +
  // role-expanded members into one User[] with @everyone-role
  // gating. Returns `massMentionDenied: true` when the user picked
  // the `@everyone` role but lacks MENTION_EVERYONE — parallel to
  // the text-path gate in #323.
  const canMentionEveryone = !!interaction.guild
    && interaction.memberPermissions?.has(PermissionFlagsBits.MentionEveryone) === true;
  // Trigger on any role pick (not just @everyone): `role.members` for a
  // non-@everyone role is a filtered view of `guild.members.cache`, so
  // both hit the same empty-cache failure mode. Direct user picks skip
  // the fetch — Discord ships the User object in `interaction.users`.
  //
  // DIVERGES from the text-path gate at handleQurlSlashSend, which
  // additionally requires `canMentionEveryone || hasRoleMention` to
  // skip the wasted fetch on bare `@everyone` typed without perm. The
  // picker omits the @everyone role from its UI (Discord platform
  // filter — out of scope here), so the only roles reaching this gate
  // are user-deliberately-picked custom roles. A wasted fetch on a
  // non-mentionable custom role pick (which the per-role
  // `role.mentionable` gate inside `resolveMentionableSelection` will
  // ultimately deny) is rare enough that the simpler trigger is worth
  // the marginal optimization loss.
  if (interaction.roles?.size > 0) {
    await prewarmGuildMembersCache(interaction.guild, { caller: 'handleConfirmUserSelect', flow_id });
  }
  const {
    users: selected,
    massMentionDenied,
    droppedFromRoles,
    everyoneCacheCold,
    roleMentionsDenied,
  } = resolveMentionableSelection({
    interaction,
    canMentionEveryone,
    flow_id,
  });
  // Resolve denied role IDs to names (with fallback for cache miss /
  // deleted role) for the warnings block. Kept caller-side so the
  // renderer stays pure of guild lookups, mirroring the text path.
  const roleMentionsDeniedNames = resolveRoleNames(interaction.guild, roleMentionsDenied);
  if (
    selected.length === 0
    && !massMentionDenied
    && droppedFromRoles === 0
    && !everyoneCacheCold
    && roleMentionsDenied.length === 0
  ) {
    // Truly empty pick (no users, no roles, no @everyone, no
    // bot-only roles, cache not cold) → already acked via deferUpdate
    // above. Other empty-selected cases fall through to the
    // all-invalid branch below so the user sees a reason banner.
    return undefined;
  }
  // `interaction.user.id` IS the original sender's ID — Discord
  // enforces "only the user who triggered an ephemeral interaction
  // can click its components" at the gateway level, so the clicker
  // === the sender. This invariant is LOAD-BEARING: partitioning
  // by clicker is how we detect `selfIncluded` (sender appears in
  // the pick), and a future refactor that flips the confirm card
  // to non-ephemeral would need to thread the original sender ID
  // through the flow payload + read it here instead.
  //
  // No `resolveRecipientUsers` re-fetch here — Discord's
  // MentionableSelectMenu only surfaces users + roles visible to
  // the bot in this guild, so picked IDs are guild-bounded at the
  // gateway-event level. Role expansion happens inline against
  // `role.members` / `guild.members.cache` (already populated for
  // any user the bot can see). handleConfirmSendClick re-fetches at
  // click time (partial-drop test pins this) as the actual guild-
  // membership defense; adding it here would burn members.fetch
  // calls per picker tick without catching anything the Send-time
  // check misses.
  const payload = row.payload || {};
  const { valid, droppedBots, selfIncluded } = partitionRecipients(selected, interaction.user.id);
  // Both invalid-pick branches re-render the full confirm card with a
  // warning banner prepended. Replacing card content with just the
  // warning string strips the resource header ("Sending file:
  // report.pdf / Expires: 24h / Self-destruct: …") that the user
  // chose at /qurl send time — they shouldn't have to scroll back to
  // remember what they're sending. needsPicker:true keeps the "Pick
  // recipients below" prompt; sendDisabled:true keeps Send greyed.
  const rejectPick = (warning) => interaction.editReply({
    content: renderConfirmCardContent({
      resourceType: payload.resourceType,
      resourceLabel: payload.resourceLabel,
      validRecipients: [],
      expiresIn: payload.expiresIn,
      selfDestructSeconds: payload.selfDestructSeconds,
      personalMessage: payload.personalMessage,
      warningsBlock: warning,
      needsPicker: true,
      interaction,
      recipientMode: RECIPIENT_MODE_PICKER,
      voiceChannelId: payload.voiceChannelId,
    }),
    components: renderConfirmCardRows({
      sendDisabled: true,
      expiresIn: payload.expiresIn,
      selfDestructSeconds: payload.selfDestructSeconds,
      personalMessage: payload.personalMessage,
      voiceChannelId: payload.voiceChannelId,
      interaction,
      // Mirror payload.recipientIds (unchanged on the reject branch — no
      // transitionFlow fires here) so the user can recover their prior
      // valid selection on re-open instead of starting from empty.
      recipientIds: payload.recipientIds || [],
      recipientMode: RECIPIENT_MODE_PICKER,
    }),
  }).catch(logIgnoredDiscordErr);
  if (valid.length === 0) {
    // No transitionFlow on the all-invalid branch — the card stays at
    // the same flow_state version. Log for forensics so a sudden spike
    // in invalid-pick churn surfaces in metrics without lowering log
    // verbosity.
    logger.debug('handleConfirmUserSelect: all-invalid pick', {
      flow_id,
      dropped_bots: droppedBots,
      mass_mention_denied: massMentionDenied,
      dropped_from_roles: droppedFromRoles,
      everyone_cache_cold: everyoneCacheCold,
      role_mentions_denied: roleMentionsDenied.length,
    });
    // Reachable cases:
    //   - bot-only individual pick (droppedBots > 0)
    //   - picked the @everyone role without MENTION_EVERYONE perm
    //     (massMentionDenied === true)
    //   - picked a role whose only members are bots
    //     (droppedFromRoles > 0, selected.length === 0)
    //   - picked @everyone WITH perm but guild.members.cache is
    //     missing / empty (everyoneCacheCold === true) — typically
    //     right after a bot restart, before chunk-on-startup lands
    // List-builder pattern; the fallback prevents a degraded
    // `⚠ . Re-pick...` message if all conditions are false.
    // Multiple reasons can fire together: e.g. directly-picked bot
    // and a bot-only role both surface since they describe
    // independent picker actions.
    //
    // COPY PARITY: the partial-valid path uses renderRecipientWarnings
    // for the same set of signals but with longer bulleted copy
    // (warnings block vs. rejection banner — different UI contexts).
    // When adding a new reason, update BOTH surfaces.
    // Reason ordering mirrors renderRecipientWarnings's bullet
    // ordering (droppedBots → droppedFromRoles → everyoneCacheCold →
    // massMentionDenied → roleMentionsDenied) so the two surfaces
    // present multi-signal picks in the same sequence. Most picks
    // hit at most one signal so ordering is rarely user-visible,
    // but pin the parity here against future drift.
    const reasons = [];
    if (droppedBots > 0) reasons.push(RECIPIENT_REASON_BOTS_DROPPED);
    if (droppedFromRoles > 0) reasons.push('Picked role(s) have no non-bot members');
    if (everyoneCacheCold) reasons.push('Member cache not yet ready — try again in a few seconds');
    if (massMentionDenied) reasons.push('`@everyone` requires the **Mention Everyone** permission');
    if (roleMentionsDenied.length > 0) {
      // Condensed copy (count, not per-role) for the rejection banner —
      // the warnings block (renderRecipientWarnings) does per-role
      // bullets. Different UI contexts, COPY PARITY noted in
      // renderRecipientWarnings's docstring. Surfaces the
      // `role.mentionable: true` bypass inline so a user who hits
      // this banner knows the workaround without having to find the
      // per-role copy (which only the partial-valid surface renders).
      // Phrase the bypass as "have the role marked" rather than
      // "mark the role" — by definition the user hitting this banner
      // lacks MENTION_EVERYONE and likely lacks Manage Roles too, so
      // imperative phrasing reads as misleading agency. The workaround
      // ("ask an admin") is real but indirect; reflect that in the copy.
      // Singular/plural noun AND verb stay in lockstep so the single-
      // role case ("Non-mentionable role requires …") doesn't render
      // as a noun/verb mismatch — most one-role picks hit this banner.
      const isSingular = roleMentionsDenied.length === 1;
      const noun = isSingular ? 'role' : 'roles';
      const verb = isSingular ? 'requires' : 'require';
      reasons.push(`Non-mentionable ${noun} ${verb} the **Mention Everyone** permission (or have the role marked as mentionable)`);
    }
    const reasonText = reasons.length > 0 ? reasons.join('. ') : 'No usable recipients in pick';
    return rejectPick(`⚠\u{FE0F} ${reasonText}. Re-pick recipients below.\n\n`);
  }
  // Defense-in-depth — unreachable in production today: the picker's
  // setMaxValues caps at min(USER_SELECT_PER_PICK_CAP=25,
  // QURL_SEND_MAX_RECIPIENTS) and Discord's own hard cap on max_values
  // is 25, so the user physically can't pick more than 25. Kept against
  // a future change to either constant (or a forged interaction) so the
  // cap stays honored.
  if (valid.length > config.QURL_SEND_MAX_RECIPIENTS) {
    return rejectPick(`⚠\u{FE0F} Pick at most ${config.QURL_SEND_MAX_RECIPIENTS} recipients.\n\n`);
  }

  // Recompute warnings + aliases for the new pick. droppedBots,
  // selfIncluded, and massMentionDenied can all flip here (the
  // picker doesn't pre-exclude the invoker, bots, or the @everyone
  // role), so warnings + the self-notice change with the recipient
  // set. DM suppression isn't needed — `canMentionEveryone` already
  // requires `interaction.guild` (above), so a DM interaction never
  // reaches a state where `massMentionDenied` could be true.
  const newWarningsBlock = renderRecipientWarnings({
    droppedBots,
    droppedFromRoles,
    massMentionDenied,
    everyoneCacheCold,
    roleMentionsDeniedNames,
  });
  const newRecipientAliases = Object.fromEntries(
    valid.map((u) => [u.id, resolveRecipientAlias(u, interaction)])
  );
  // Asymmetry vs. the menu/modal handlers' no-op short-circuits:
  // the picker DOES NOT skip transitionFlow on a same-recipient-set
  // re-pick. Re-running here recomputes warningsBlock (droppedBots
  // can flip if the picker selection now includes a bot) plus
  // selfIncluded (flips if sender enters or leaves the pick), and
  // refreshes recipientAliases (display names may have changed
  // since the prior pick). All three are user-visible and worth
  // the version bump.
  const newPayload = {
    ...payload,
    recipientIds: valid.map((u) => u.id),
    recipientAliases: newRecipientAliases,
    warningsBlock: newWarningsBlock,
    selfIncluded,
    // Picker activity definitionally lands the card in picker-mode.
    // If the user clicked the picker (which is the only entry to this
    // handler), they are NOT in voice or everyone mode — even if the
    // inbound payload still carried `'voice'` / `'everyone'` from a
    // stale superseded flow (picker is hidden in those modes, but a
    // forged interaction or stale UI could still emit the event).
    // Explicit override here keeps a `...payload` spread from leaking
    // a non-picker mode forward.
    recipientMode: RECIPIENT_MODE_PICKER,
  };
  // Targeted catch around transitionFlow mirrors the same shape
  // handleConfirmSendClick / handleConfirmCancelClick use around their
  // DDB calls. A throw here would otherwise bubble to the dispatcher's
  // outer catch which surfaces a generic "superseded" message —
  // wrong, since nothing was actually superseded on a DDB blip.
  let result;
  try {
    result = await transitionFlow(flow_id, row.version, {
      stage_to: SEND_STAGE_AWAITING_CONFIRM,
      payload: newPayload,
      terminal: false,
      set_expires_at: Math.floor(Date.now() / 1000) + SEND_FLOW_TTL_SECONDS,
    });
  } catch (err) {
    logger.error('handleConfirmUserSelect: transitionFlow threw', {
      flow_id, error: err && err.message,
    });
    return interaction.followUp({
      content: '❌ Could not save your pick right now. Try selecting recipients again in a moment.',
      ephemeral: true,
    }).catch(logIgnoredDiscordErr);
  }
  if (result.result === 'conflict') {
    return interaction.editReply({
      content: 'Send was superseded — re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }
  if (result.result === 'not_found') {
    return interaction.editReply({
      content: 'This send expired — re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }

  // The CONTENT renders the resolved recipient summary
  // ("**To:** N user(s)") with needsPicker:false; the ROWS keep the
  // picker attached in picker-mode so the user can re-pick. Send is
  // enabled now that we have a valid recipient set.
  const content = renderConfirmCardContent({
    resourceType: payload.resourceType,
    resourceLabel: payload.resourceLabel,
    validRecipients: valid,
    expiresIn: payload.expiresIn,
    selfDestructSeconds: payload.selfDestructSeconds,
    personalMessage: payload.personalMessage,
    warningsBlock: newWarningsBlock,
    needsPicker: false,
    interaction,
    selfIncluded,
    recipientMode: RECIPIENT_MODE_PICKER,
    voiceChannelId: payload.voiceChannelId,
  });
  return interaction.editReply({
    content,
    components: renderConfirmCardRows({
      sendDisabled: false,
      expiresIn: payload.expiresIn,
      selfDestructSeconds: payload.selfDestructSeconds,
      personalMessage: payload.personalMessage,
      voiceChannelId: payload.voiceChannelId,
      interaction,
      recipientIds: newPayload.recipientIds,
      recipientMode: RECIPIENT_MODE_PICKER,
    }),
  }).catch(logIgnoredDiscordErr);
}

// Confirm-card "Everyone on voice" button. Mirrors the
// UserSelectMenu handler's shape (deferUpdate → resolve → partition →
// transitionFlow → re-render) but reads the voice-connected member
// set AT CLICK TIME from the guild's voice-state cache rather than
// taking the selection from a Discord-emitted picker payload. This
// click-time read is load-bearing: a 30s-old snapshot would silently
// send to users who already left voice.
//
// The button is only rendered when payload.voiceChannelId is set
// (snapshot at slash-command time). A click without that field is
// either a forged interaction or a malformed flow row — treat as a
// corrupt payload and surface re-run copy, same shape as
// handleConfirmUserSelect's resourceType guard.
async function handleConfirmVoiceEveryone(interaction, { flow_id, row }) {
  await interaction.deferUpdate().catch(logIgnoredDiscordErr);

  const payload = row.payload || {};
  // resourceType guard mirrors handleConfirmUserSelect — a corrupt /
  // stale row would otherwise throw from renderConfirmCardContent.
  const payloadResource = payload.resourceType;
  if (payloadResource !== RESOURCE_TYPES.FILE && payloadResource !== RESOURCE_TYPES.MAPS) {
    logger.error('handleConfirmVoiceEveryone: corrupt flow payload (unknown resourceType)', {
      flow_id, resource_type: payloadResource,
    });
    // deleteFlow is best-effort here — `.catch(logIgnoredDiscordErr)`
    // swallows transient DDB failures so a flow-state-write blip
    // doesn't block the user-visible error. The row will TTL out
    // (3-min SEND_FLOW_TTL_SECONDS) even if this delete misses; the
    // user-facing "Card data is corrupted" message lands either way.
    await deleteFlow(flow_id, {
      stage: SEND_STAGE_AWAITING_CONFIRM,
      reason: 'terminal',
    }).catch(logIgnoredDiscordErr);
    return interaction.editReply({
      content: '❌ Card data is corrupted — please re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }

  const voiceChannelId = payload.voiceChannelId;
  if (!voiceChannelId) {
    // Button shouldn't have rendered without this field. A click here
    // is either a forged interaction or a payload that lost the field
    // (schema drift). Re-render the card WITHOUT the voice button
    // (rerenderConfirmCard's renderConfirmCardRows conditions on
    // voiceChannelId being set) so the broken affordance is removed
    // AND the user gets visible feedback that something changed —
    // silently absorbing the click post-deferUpdate would leave the
    // user staring at an unchanged card with no indication their
    // click registered.
    //
    // Reuses `rerenderConfirmCard` (instead of an inline rebuild) so
    // any previously-picked recipients in the payload still show in
    // the re-rendered card — the inline-rebuild alternative would
    // surface validRecipients:[] in the UI while the persisted
    // payload still carried the old recipientIds, masking real
    // bugs as a UI/state inconsistency.
    logger.warn('handleConfirmVoiceEveryone: voice button click against payload with no voiceChannelId', {
      flow_id, interaction_id: interaction.id,
    });
    // Surface a warning banner alongside the re-render so the user
    // can tell the voice button vanished intentionally (vs. an
    // unexplained UI flicker). The button removal is automatic via
    // `voiceChannelId: null` flowing through to renderConfirmCardRows.
    return rerenderConfirmCard(interaction, {
      ...payload,
      voiceChannelId: null,
      warningsBlock: VOICE_REJECT_CONTEXT_LOST,
    });
  }

  // Re-render with a warning banner. Same shape as the picker's
  // rejectPick path so the user sees why the button didn't take.
  // Pass the local `voiceChannelId` (already proven truthy above) for
  // consistency with the other handler reads below.
  //
  // The payload keeps `voiceChannelId` set even on the reject path —
  // intentionally. A transient cache miss (voice intent dropped,
  // members momentarily evicted) will re-disable the button via the
  // count=null branch on this re-render, but a subsequent picker /
  // expiry / note interaction will run renderConfirmCardRows again
  // with the now-repopulated cache and re-enable the button. Recovery
  // is automatic; persisting the field is the load-bearing piece.
  //
  // No `transitionFlow` call here either — the persisted
  // `recipientIds` from the original slash command (if any) stays on
  // the row. Send is disabled in this re-render (sendDisabled:true)
  // so the stale recipientIds can't fan out; the picker re-pick
  // path is the natural way to refresh them. Matches `rejectPick`'s
  // pattern in handleConfirmUserSelect.
  // Reject paths render in picker-mode regardless of the inbound
  // `payload.recipientMode` — the warning copy ("Pick recipients
  // below.") wires the user toward the picker, so the picker row must
  // be present. The flow_state row's `recipientMode` is NOT updated
  // here (no transitionFlow fires on reject); a subsequent picker /
  // expiry / note interaction will re-derive layout from whatever
  // mode the payload still carries.
  const rejectVoice = (warning) => interaction.editReply({
    content: renderConfirmCardContent({
      resourceType: payload.resourceType,
      resourceLabel: payload.resourceLabel,
      validRecipients: [],
      expiresIn: payload.expiresIn,
      selfDestructSeconds: payload.selfDestructSeconds,
      personalMessage: payload.personalMessage,
      warningsBlock: warning,
      needsPicker: true,
      interaction,
      recipientMode: RECIPIENT_MODE_PICKER,
      voiceChannelId,
    }),
    components: renderConfirmCardRows({
      sendDisabled: true,
      expiresIn: payload.expiresIn,
      selfDestructSeconds: payload.selfDestructSeconds,
      personalMessage: payload.personalMessage,
      voiceChannelId,
      interaction,
      // Mirror payload.recipientIds (unchanged on the reject branch —
      // no transitionFlow fires) so the user can recover their prior
      // valid selection on re-open of the picker.
      recipientIds: payload.recipientIds || [],
      recipientMode: RECIPIENT_MODE_PICKER,
    }),
  }).catch(logIgnoredDiscordErr);

  // Resolve voice-connected members AT CLICK TIME from the voice-state
  // cache (populated by the GuildVoiceStates intent). channel.members
  // is a Collection<Snowflake, GuildMember> filtered to currently-
  // connected members for voice/stage-voice channels. A cache miss
  // (channel was deleted between render and click, voice intent was
  // recently dropped, etc.) lands in the warning branch.
  const channel = interaction.guild?.channels?.cache?.get?.(voiceChannelId);
  if (!channel || !channel.members) {
    return rejectVoice(VOICE_REJECT_CHANNEL_UNREADABLE);
  }
  // True-empty channel gets honest "no one connected" copy.
  // bots-only flows through partition below and surfaces "Cannot send
  // to bots" — that copy depends on partitionRecipients owning the
  // bot filter end-to-end (a pre-filter here would zero out droppedBots
  // and force the bots-only case into a misleading "no usable
  // recipients" fallback).
  if (channel.members.size === 0) {
    return rejectVoice(VOICE_REJECT_EMPTY_CHANNEL);
  }
  // GuildMember → User shape so partitionRecipients sees the same
  // shape the picker hands it. Skip entries with no .user (defensive
  // against partial-cache rows); partitionRecipients handles the
  // bot filter + droppedBots accounting.
  //
  // Partial-cache drops are logged at debug level — silent shrinkage
  // of the recipient set is hard to diagnose post-hoc without
  // telemetry. The log fires per shrunk send (not per dropped
  // member) so volume tracks frequency of the degraded state, not
  // member count.
  const selectedUsers = [];
  let partialCacheDrops = 0;
  for (const [, m] of channel.members) {
    if (m?.user) selectedUsers.push(m.user);
    else partialCacheDrops++;
  }
  if (partialCacheDrops > 0) {
    logger.debug('handleConfirmVoiceEveryone: partial-cache rows dropped from voice resolution', {
      flow_id, voice_channel_id: voiceChannelId, dropped: partialCacheDrops,
      channel_size: channel.members.size,
    });
  }
  // Sender is excluded from the voice-everyone set: clicking "Everyone
  // in #voice" semantically means "everyone else in the room," not
  // "and CC myself." This matches the slash-command auto-default at
  // handleQurlSlashSend's voice-mode override.
  const { valid, droppedBots } = partitionRecipients(
    selectedUsers, interaction.user.id, { excludeSender: true }
  );
  if (valid.length === 0) {
    // Reached when every connected member was filtered out — today
    // that's the bots-only case (droppedBots > 0), the sender-only
    // case (sender is the only non-bot in voice), or both. The fallback
    // text covers a future filter addition (e.g., role-blocklist)
    // that drops everyone for a different reason.
    const reasons = [];
    if (droppedBots > 0) reasons.push(RECIPIENT_REASON_BOTS_DROPPED);
    const reasonText = reasons.length > 0
      ? reasons.join('. ')
      : "You're the only one in this voice channel";
    return rejectVoice(`⚠\u{FE0F} ${reasonText}. Pick recipients below.\n\n`);
  }
  // The 20k QURL_SEND_MAX_RECIPIENTS default is sized to accommodate
  // voice/stage capacity (Discord's voice channel caps at 99; stage
  // channels are higher), so a hard cap rejection here is defense-in-
  // depth against a misconfigured smaller env override rather than the
  // common path. The picker's similar guard at handleConfirmUserSelect
  // is identically structured.
  //
  // CAP DIVERGENCE (intentional): the parser's `<#voice>` path
  // truncates at cap (silent partial-resolution with `cappedCount`
  // surfaced), while this button path hard-rejects past cap. Different
  // UX shapes for the same conceptual input ("everyone in voice")
  // is acceptable because:
  //   - The button is unambiguous "all-or-nothing" intent — a partial
  //     fan-out misrepresents the click.
  //   - The parser path mixes channel mentions with other mentions,
  //     where partial-resolution is the existing pattern.
  //   - The reject is unreachable under defaults (voice caps < 20k);
  //     only env-overridden smaller caps trip it.
  // Tracked in #339 if a future product decision needs to align
  // these surfaces (e.g., button truncates with confirm-card warning).
  if (valid.length > config.QURL_SEND_MAX_RECIPIENTS) {
    // "eligible recipients" (NOT "connected") — `valid.length` is the
    // post-partition count (sender + bots already filtered out).
    // Discord's voice panel shows raw connections, so phrasing this as
    // "connected" would diverge from what the user sees there. Stays
    // in lockstep with the slash-entry over-cap banner.
    return rejectVoice(`⚠\u{FE0F} Voice channel has ${valid.length} eligible recipients (max ${config.QURL_SEND_MAX_RECIPIENTS}). Use the picker or @mentions to choose a subset.\n\n`);
  }

  const newWarningsBlock = renderRecipientWarnings({
    droppedBots,
  });
  const newRecipientAliases = Object.fromEntries(
    valid.map((u) => [u.id, resolveRecipientAlias(u, interaction)])
  );
  const newPayload = {
    ...payload,
    recipientIds: valid.map((u) => u.id),
    recipientAliases: newRecipientAliases,
    warningsBlock: newWarningsBlock,
    // Sender was excluded in `partitionRecipients` above with
    // `{ excludeSender: true }`, so `selfIncluded` is structurally
    // false on this path. Explicit so a future refactor that drops
    // the option can't silently flip the flag back to true via the
    // `...payload` spread carrying a stale value forward.
    selfIncluded: false,
    // Voice-mode commits the layout: picker row disappears, bottom
    // row swaps in the "👥 Pick people instead" escape hatch.
    recipientMode: RECIPIENT_MODE_VOICE,
  };
  let result;
  try {
    result = await transitionFlow(flow_id, row.version, {
      stage_to: SEND_STAGE_AWAITING_CONFIRM,
      payload: newPayload,
      terminal: false,
      set_expires_at: Math.floor(Date.now() / 1000) + SEND_FLOW_TTL_SECONDS,
    });
  } catch (err) {
    logger.error('handleConfirmVoiceEveryone: transitionFlow threw', {
      flow_id, error: err && err.message,
    });
    return interaction.followUp({
      content: '❌ Could not save your selection right now. Try again in a moment.',
      ephemeral: true,
    }).catch(logIgnoredDiscordErr);
  }
  if (result.result === 'conflict') {
    return interaction.editReply({
      content: 'Send was superseded — re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }
  if (result.result === 'not_found') {
    return interaction.editReply({
      content: 'This send expired — re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }

  // Audit-log parity with handleConfirmEveryone — voice fan-outs
  // need to be findable in logs without a qurl_send_configs scan.
  // Asymmetry: no guild-total peer at voice scope (channel.userLimit
  // is a capacity cap, not a count), so one channel-size field —
  // (voice_member_count - valid_count) = partial-cache + filter gap.
  logger.info('handleConfirmVoiceEveryone: voice @everyone expansion succeeded', {
    flow_id, guild_id: interaction.guildId, user_id: interaction.user.id,
    voice_channel_id: voiceChannelId,
    valid_count: valid.length, dropped_bots: droppedBots,
    partial_cache_drops: partialCacheDrops, self_included: false,
    voice_member_count: channel.members.size,
  });

  const content = renderConfirmCardContent({
    resourceType: payload.resourceType,
    resourceLabel: payload.resourceLabel,
    validRecipients: valid,
    expiresIn: payload.expiresIn,
    selfDestructSeconds: payload.selfDestructSeconds,
    personalMessage: payload.personalMessage,
    warningsBlock: newWarningsBlock,
    needsPicker: false,
    interaction,
    selfIncluded: false,
    recipientMode: RECIPIENT_MODE_VOICE,
    voiceChannelId,
  });
  return interaction.editReply({
    content,
    components: renderConfirmCardRows({
      sendDisabled: false,
      expiresIn: payload.expiresIn,
      selfDestructSeconds: payload.selfDestructSeconds,
      personalMessage: payload.personalMessage,
      voiceChannelId,
      interaction,
      recipientIds: newPayload.recipientIds,
      recipientMode: RECIPIENT_MODE_VOICE,
    }),
  }).catch(logIgnoredDiscordErr);
}

// Confirm-card "👥 Pick people instead" button — escape hatch from
// both voice-mode and everyone-mode back to picker-mode. Flips
// `recipientMode` to 'picker', clears recipientIds (the voice- or
// guild-resolved set was authored for "everyone," not a starting
// point for hand-curation; carrying it forward would land the user
// in picker-mode with the population pre-selected, which is a
// confusing "did my switch take?" state — and a 25-entry truncated
// view of any larger set).
//
// No resource resolution / member lookups happen here — purely a UI
// mode toggle. transitionFlow still fires so the persisted payload
// matches what the card shows; otherwise a subsequent expiry / note
// re-render would re-derive picker layout from a payload that still
// carried the prior mode and snap back.
async function handleConfirmPickManual(interaction, { flow_id, row }) {
  await interaction.deferUpdate().catch(logIgnoredDiscordErr);

  const payload = row.payload || {};
  // resourceType guard mirrors the other confirm-card handlers — a
  // corrupt or stale row would otherwise throw inside
  // renderConfirmCardContent's resource-type switch.
  const payloadResource = payload.resourceType;
  if (payloadResource !== RESOURCE_TYPES.FILE && payloadResource !== RESOURCE_TYPES.MAPS) {
    logger.error('handleConfirmPickManual: corrupt flow payload (unknown resourceType)', {
      flow_id, resource_type: payloadResource,
    });
    await deleteFlow(flow_id, {
      stage: SEND_STAGE_AWAITING_CONFIRM,
      reason: 'terminal',
    }).catch(logIgnoredDiscordErr);
    return interaction.editReply({
      content: '❌ Card data is corrupted — please re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }

  // Drop recipientIds (and the voice-derived aliases) so the picker
  // re-renders with the "Pick recipients below" prompt + Send disabled
  // — the user is starting recipient selection over by definition of
  // having clicked "Pick people instead."
  const newPayload = {
    ...payload,
    recipientIds: [],
    recipientAliases: {},
    recipientMode: RECIPIENT_MODE_PICKER,
    selfIncluded: false,
    // Voice-mode's warningsBlock (e.g. droppedBots from voice members)
    // doesn't apply once the user opts into manual selection — the
    // picker will produce its own warningsBlock on the next pick.
    warningsBlock: '',
  };
  let result;
  try {
    result = await transitionFlow(flow_id, row.version, {
      stage_to: SEND_STAGE_AWAITING_CONFIRM,
      payload: newPayload,
      terminal: false,
      set_expires_at: Math.floor(Date.now() / 1000) + SEND_FLOW_TTL_SECONDS,
    });
  } catch (err) {
    logger.error('handleConfirmPickManual: transitionFlow threw', {
      flow_id, error: err && err.message,
    });
    return interaction.followUp({
      content: '❌ Could not switch to manual picker right now. Try again in a moment.',
      ephemeral: true,
    }).catch(logIgnoredDiscordErr);
  }
  if (result.result === 'conflict') {
    return interaction.editReply({
      content: 'Send was superseded — re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }
  if (result.result === 'not_found') {
    return interaction.editReply({
      content: 'This send expired — re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }
  return rerenderConfirmCard(interaction, newPayload);
}

// @everyone button — workaround for Discord's MentionableSelectMenu
// filtering out the @everyone role from its dropdown. Click-time
// semantics mirror handleConfirmUserSelect's @everyone-role branch:
// pre-warm cache → expand to all non-bot guild members → partition →
// transition flow. Permission re-check is defense-in-depth against a
// forged button click (renderConfirmCardRows already gates button
// visibility on MENTION_EVERYONE + picker-mode; this defends against
// a crafted HTTP interaction bypassing the render).
async function handleConfirmEveryone(interaction, { flow_id, row }) {
  await interaction.deferUpdate().catch(logIgnoredDiscordErr);

  const payload = row.payload || {};
  const payloadResource = payload.resourceType;
  if (payloadResource !== RESOURCE_TYPES.FILE && payloadResource !== RESOURCE_TYPES.MAPS) {
    logger.error('handleConfirmEveryone: corrupt flow payload (unknown resourceType)', {
      flow_id, resource_type: payloadResource,
    });
    await deleteFlow(flow_id, {
      stage: SEND_STAGE_AWAITING_CONFIRM,
      reason: 'terminal',
    }).catch(logIgnoredDiscordErr);
    return interaction.editReply({
      content: '❌ Card data is corrupted — please re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }

  // Symmetric with handleConfirmVoiceEveryone's rejectVoice — same
  // surface shape (warning banner + re-render with needsPicker). The
  // persisted recipientIds stay unchanged on rejection (no transition-
  // Flow fires); the picker re-pick is the natural recovery.
  const rejectEveryone = (warning) => interaction.editReply({
    content: renderConfirmCardContent({
      resourceType: payload.resourceType,
      resourceLabel: payload.resourceLabel,
      validRecipients: [],
      expiresIn: payload.expiresIn,
      selfDestructSeconds: payload.selfDestructSeconds,
      personalMessage: payload.personalMessage,
      warningsBlock: warning,
      needsPicker: true,
      interaction,
      recipientMode: RECIPIENT_MODE_PICKER,
      voiceChannelId: payload.voiceChannelId,
    }),
    components: renderConfirmCardRows({
      sendDisabled: true,
      expiresIn: payload.expiresIn,
      selfDestructSeconds: payload.selfDestructSeconds,
      personalMessage: payload.personalMessage,
      voiceChannelId: payload.voiceChannelId,
      interaction,
      recipientIds: payload.recipientIds || [],
      recipientMode: RECIPIENT_MODE_PICKER,
    }),
  }).catch(logIgnoredDiscordErr);

  // Defense-in-depth permission re-check. The render-time gate already
  // hides the button without MENTION_EVERYONE, but a forged HTTP
  // interaction could carry the custom_id without the underlying perm.
  // Channel-effective perms (interaction.memberPermissions) — same as
  // the text-path gate at handleQurlSlashSend.
  const canMentionEveryone = !!interaction.guild
    && interaction.memberPermissions?.has?.(PermissionFlagsBits.MentionEveryone) === true;
  if (!canMentionEveryone) {
    logger.warn('handleConfirmEveryone: click without MENTION_EVERYONE — likely forged', {
      flow_id, interaction_id: interaction.id,
      // `guild_id` for cross-line correlation with the success info
      // log; `null` in DM context which is itself the forge signal.
      guild_id: interaction.guildId ?? null,
    });
    return rejectEveryone('⚠\u{FE0F} `@everyone` requires the **Mention Everyone** permission.\n\n');
  }

  await prewarmGuildMembersCache(interaction.guild, { caller: 'handleConfirmEveryone', flow_id });

  // Include bots in selectedUsers and let partitionRecipients filter
  // them — same pattern as handleConfirmVoiceEveryone. Without this,
  // the bots-only case would surface as "cache not ready" (misleading)
  // instead of "all recipients are bots" (accurate); droppedBots count
  // is also load-bearing for the warning copy.
  //
  // NO early break on `selectedUsers.length > cap` (rejected). Such a
  // break is unsound under bot-clustered iteration order: a 20001-human
  // guild with bots iterated around the cap boundary would early-exit
  // before the (cap+1)th HUMAN, producing `valid.length === cap` and
  // silently bypassing the cap-reject below — the send would proceed
  // at exactly the cap with the over-cap members invisibly dropped.
  // Full-cache iteration costs O(cache_size) (~10ms even at 100k),
  // which is acceptable post-deferUpdate; correctness wins.
  //
  // partial-cache rows (member without `.user` — degraded shape from
  // a partially-loaded `GUILD_MEMBERS_CHUNK`) are silently filtered
  // and counted for forensics, matching `handleConfirmVoiceEveryone`'s
  // `partialCacheDrops` telemetry so a future "@everyone under-
  // resolved on guild X" report has a debug hook.
  const selectedUsers = [];
  let partialCacheDrops = 0;
  let senderInCache = false;
  let hasOtherNonBotInCache = false;
  const cache = interaction.guild.members?.cache;
  // Guard matches the operation: `for ... of` reads `[Symbol.iterator]`,
  // which both `Map` and discord.js `Collection` provide. Truthy-check
  // alone would NPE on a degraded `{}` cache shape from a partial init.
  if (cache && typeof cache[Symbol.iterator] === 'function') {
    for (const [, member] of cache) {
      if (member?.user) {
        selectedUsers.push(member.user);
        if (member.user.id === interaction.user.id) senderInCache = true;
        else if (!member.user.bot) hasOtherNonBotInCache = true;
      } else {
        partialCacheDrops++;
      }
    }
  }
  if (partialCacheDrops > 0) {
    logger.debug('handleConfirmEveryone: partial-cache rows dropped from @everyone resolution', {
      flow_id, guild_id: interaction.guildId,
      dropped: partialCacheDrops, cache_size: cache?.size,
    });
  }
  if (selectedUsers.length === 0) {
    // "member cache not ready" is the common case (cold cache + non-
    // zero guild). The degenerate genuinely-empty guild
    // (`memberCount === 0`) can't reach this branch in production —
    // handleQurlSlashSend's guild-gate rejects DM at entry and a
    // guild with zero members can't host a slash command anyway. The
    // shared copy is acceptable.
    // Important: gate the defensive sender push below on cache having
    // members. A truly empty cache should surface "cache not ready"
    // rather than silently expanding to just the sender — the latter
    // would mislead the user into thinking @everyone succeeded.
    return rejectEveryone('⚠\u{FE0F} `@everyone` expanded to 0 recipients — member cache not ready. Try again in a moment.\n\n');
  }
  // Defensive sender push: cache has OTHER non-bot members but the
  // sender's row is somehow missing (rare race during shard resume /
  // partial chunk delivery post-prewarm). Without this, clicking
  // 📢 `@everyone` could land `selfIncluded: false` and the user
  // wouldn't be in the recipient set they explicitly triggered.
  //
  // Gated on `hasOtherNonBotInCache` so a bots-only cache still
  // surfaces the bots-only reject below — silently expanding to
  // "send to just me" from a bots-only guild misrepresents the
  // @everyone click. `partitionRecipients` does NOT re-dedup
  // (contract at commands.js:~3077), so the `senderInCache` check is
  // load-bearing — pushing unconditionally would land the sender
  // twice in `valid`.
  if (!senderInCache && hasOtherNonBotInCache) {
    // `interaction.user.bot` may be undefined depending on interaction
    // shape (slash commands can't come from bots in production, so
    // `partitionRecipients`'s `if (u.bot)` is a no-op here regardless).
    // Falsy `.bot` → kept; the invariant holds across context-menu /
    // future interaction types too.
    selectedUsers.push(interaction.user);
  }
  // Sender-only degenerate: guild has the sender (+ optionally bots)
  // but no other non-bot members. @everyone's click intent is "fan out
  // to OTHERS"; falling through to partition would land `valid=[sender]`
  // and silently send to just the sender — defensible self-send
  // semantics, but misleading for a click labeled 📢 @everyone. Reject
  // with explicit copy so the user can decide (self-send via the
  // picker is still possible). Reached only when sender's row IS in
  // cache and no other non-bots are present — the bots-only-no-sender
  // case lands in `valid.length === 0` below with droppedBots > 0.
  if (senderInCache && !hasOtherNonBotInCache) {
    return rejectEveryone('⚠\u{FE0F} `@everyone` matched only you — no other non-bot members to send to.\n\n');
  }
  // DIVERGENCE FROM handleConfirmVoiceEveryone: that handler excludes
  // the sender (voice-mode semantic is "everyone else in this voice
  // channel"). The @everyone button intentionally INCLUDES the sender
  // — it's the dropdown's "select all" shortcut, not a separate "all
  // others" affordance. A future contributor aligning the two handlers
  // would silently flip the self-inclusion behavior; the `selfIncluded`
  // flag flows through to the card's "Send includes you." notice so
  // the user sees they're in the recipient list.
  const { valid, droppedBots, selfIncluded } = partitionRecipients(selectedUsers, interaction.user.id);
  if (valid.length === 0) {
    const reasons = [];
    if (droppedBots > 0) reasons.push(RECIPIENT_REASON_BOTS_DROPPED);
    const reasonText = reasons.length > 0 ? reasons.join('. ') : 'No usable recipients';
    return rejectEveryone(`⚠\u{FE0F} ${reasonText}. Pick recipients below.\n\n`);
  }
  // Cap rejection mirrors handleConfirmVoiceEveryone: hard-reject past
  // QURL_SEND_MAX_RECIPIENTS rather than silently truncate — the click
  // is unambiguous all-or-nothing intent.
  if (valid.length > config.QURL_SEND_MAX_RECIPIENTS) {
    return rejectEveryone(`⚠\u{FE0F} Guild has ${valid.length} non-bot members (max ${config.QURL_SEND_MAX_RECIPIENTS}). Use the picker or @mentions to choose a subset.\n\n`);
  }

  const newWarningsBlock = renderRecipientWarnings({ droppedBots });
  const newRecipientAliases = Object.fromEntries(
    valid.map((u) => [u.id, resolveRecipientAlias(u, interaction)])
  );
  // Mode switches to EVERYONE — like voice-everyone, the click is
  // unambiguous "fan out to all" intent. Picker row is hidden in the
  // re-render so the user can't accidentally read back a 25-entry
  // truncated picker selection over the @everyone fan-out (Discord's
  // MentionableSelect default_values is capped at 25, and any picker
  // interaction routes through handleConfirmUserSelect which replaces
  // recipientIds with the picker's view of the world).
  const newPayload = {
    ...payload,
    recipientIds: valid.map((u) => u.id),
    recipientAliases: newRecipientAliases,
    warningsBlock: newWarningsBlock,
    selfIncluded,
    recipientMode: RECIPIENT_MODE_EVERYONE,
  };
  let result;
  try {
    result = await transitionFlow(flow_id, row.version, {
      stage_to: SEND_STAGE_AWAITING_CONFIRM,
      payload: newPayload,
      terminal: false,
      set_expires_at: Math.floor(Date.now() / 1000) + SEND_FLOW_TTL_SECONDS,
    });
  } catch (err) {
    logger.error('handleConfirmEveryone: transitionFlow threw', {
      flow_id, error: err && err.message,
    });
    return interaction.followUp({
      content: '❌ Could not save your selection right now. Try again in a moment.',
      ephemeral: true,
    }).catch(logIgnoredDiscordErr);
  }
  if (result.result === 'conflict') {
    return interaction.editReply({
      content: 'Send was superseded — re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }
  if (result.result === 'not_found') {
    return interaction.editReply({
      content: 'This send expired — re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }

  // Success-path telemetry: a successful @everyone expansion is a
  // load-bearing audit signal (an admin fanned a send to N members
  // of the guild). Error paths already log; without this, ops can
  // only see "did someone fan out to 18k users?" via a DDB scan of
  // qurl_send_configs. Info level matches the cadence of other
  // confirm-card success signals.
  logger.info('handleConfirmEveryone: @everyone expansion succeeded', {
    flow_id, guild_id: interaction.guildId, user_id: interaction.user.id,
    valid_count: valid.length, dropped_bots: droppedBots,
    partial_cache_drops: partialCacheDrops, self_included: selfIncluded,
    // Cache shape for "@everyone underresolved on guild X" forensics —
    // makes the success line self-contained (no need to cross-reference
    // the partial-cache debug log to know the cache state at click time).
    cache_size: cache?.size, member_count: interaction.guild.memberCount,
  });

  return rerenderConfirmCard(interaction, newPayload);
}

// Shared re-render after a confirm-card menu/button updates the flow
// payload (expiry / self-destruct / note). Each menu handler builds
// `newPayload` and calls this with the post-transition `interaction` +
// payload. Keeping the re-render shape in one place avoids the three
// handlers drifting on `sendDisabled` / content flags.
//
// `sendDisabled` is derived from recipientIds — without it, a user
// who opened /qurl send without `recipients:` could change expiry
// before picking and see an enabled Send button against an empty
// recipient set.
// All four entry paths (picker, expiry, self-destruct, note modal)
// defer-ack first, then re-render via editReply.
async function rerenderConfirmCard(interaction, newPayload) {
  const recipientIds = Array.isArray(newPayload.recipientIds) ? newPayload.recipientIds : [];
  // Resolve each id through three layers, in priority order:
  //   1. members.cache — freshest data when populated
  //   2. payload.recipientAliases — persisted at pick time, survives
  //      cache eviction between pick and menu-click
  //   3. fabricated `user-${id}` fallback — last resort if both miss
  // resolveRecipientAlias does its own NFKC + bidi/zero-width strip
  // on whichever wins, so the rendered text is always sanitized.
  // The aliases in persistedAliases were ALREADY produced by
  // resolveRecipientAlias at pick time — re-running it here is a
  // no-op because NFKC + bidi/zero-width strip have fixed points
  // after one pass. If sanitize semantics ever stop being idempotent,
  // this becomes a double-sanitize bug.
  const memberCache = interaction.guild?.members?.cache;
  const persistedAliases = newPayload.recipientAliases || {};
  const validRecipients = recipientIds.map((id) => {
    // Asymmetric inputs through the same render path:
    //   - cache-hit returns a Discord.js User object (UNSANITIZED
    //     fields — username/displayName fresh from the gateway).
    //   - cache-miss returns { displayName: persistedAlias },
    //     where the alias was ALREADY sanitized at pick time by
    //     resolveRecipientAlias.
    // Both then re-flow through renderConfirmCardContent's
    // `resolveRecipientAlias` pass. The cache-hit branch sanitizes
    // fresh; the cache-miss branch is a no-op IF sanitize stays
    // idempotent (pinned by tests/sanitize.test.js's idempotence
    // block). If anyone splits these into divergent post-processing,
    // the cache-miss branch loses its protection.
    const cached = memberCache?.get?.(id);
    if (cached?.user) return cached.user;
    // Both fallback branches set `displayName` (NOT `username`) so
    // resolveRecipientAlias's precedence (displayName → globalName →
    // username → user-${id}) hits the first branch consistently.
    // Asymmetric shape would be a footgun if anyone touches that
    // precedence later.
    const alias = persistedAliases[id];
    return { id, displayName: alias || `user-${id}`, bot: false };
  });
  // recipientMode drives both row layout AND content prefix. Default
  // 'picker' for stale flow_state rows that pre-date the field (they
  // keep the legacy picker shape until they TTL out).
  const recipientMode = normalizeRecipientMode(newPayload.recipientMode);
  // `needsPicker` (the "Pick recipients below" prompt) is only shown
  // in picker-mode with no recipients yet. In voice-mode with an empty
  // recipientIds (e.g., voice channel emptied after switch), the card
  // shows "To: 0 users in #voice" — the user can click "Pick people
  // instead" to recover.
  const needsPicker = recipientMode === RECIPIENT_MODE_PICKER && recipientIds.length === 0;
  // Send-disabled is recipient-empty regardless of mode.
  const sendDisabled = recipientIds.length === 0;
  const content = renderConfirmCardContent({
    resourceType: newPayload.resourceType,
    resourceLabel: newPayload.resourceLabel,
    validRecipients,
    expiresIn: newPayload.expiresIn,
    selfDestructSeconds: newPayload.selfDestructSeconds,
    personalMessage: newPayload.personalMessage,
    warningsBlock: newPayload.warningsBlock || '',
    needsPicker,
    interaction,
    selfIncluded: newPayload.selfIncluded === true,
    recipientMode,
    voiceChannelId: newPayload.voiceChannelId,
  });
  return interaction.editReply({
    content,
    components: renderConfirmCardRows({
      sendDisabled,
      expiresIn: newPayload.expiresIn,
      selfDestructSeconds: newPayload.selfDestructSeconds,
      personalMessage: newPayload.personalMessage,
      voiceChannelId: newPayload.voiceChannelId,
      interaction,
      recipientIds,
      recipientMode,
    }),
  }).catch(logIgnoredDiscordErr);
}

// Expiry StringSelectMenu → update payload.expiresIn. Defense-in-depth
// validation against EXPIRY_LABELS mirrors handleQurlSlashSend's slash-
// option gate — Discord enforces the choice set server-side, but a
// forged interaction could land an off-set value here.
async function handleConfirmExpirySelect(interaction, { flow_id, row }) {
  // `?.[0]` is paranoia, not load-bearing — Discord guarantees
  // `values` is an array for StringSelectMenu interactions. The
  // guard catches a forged interaction missing the field entirely;
  // legitimate UI paths always provide it.
  const picked = interaction.values?.[0];
  // Validate before deferring so the forgery branch can use `reply`
  // (the cheaper, single-call ack) instead of `followUp` after a
  // wasted deferUpdate. Symmetric with the self-destruct handler.
  if (!picked || !isValidExpiry(picked)) {
    logger.warn('handleConfirmExpirySelect: forged off-set expiry value', {
      flow_id, value: truncForLog(picked),
    });
    return interaction.reply({
      content: '❌ Unrecognized expiry value. Re-pick from the list.',
      ephemeral: true,
    }).catch(logIgnoredDiscordErr);
  }
  await interaction.deferUpdate().catch(logIgnoredDiscordErr);
  const payload = row.payload || {};
  // No-op re-pick (same value as current state) → skip the DDB write
  // + version bump. A version bump would needlessly fence any
  // concurrent sibling interaction (picker / self-destruct / note
  // modal) that's mid-flight. Still re-render the card so the user
  // gets visible feedback that their click registered.
  if (picked === payload.expiresIn) {
    return rerenderConfirmCard(interaction, payload);
  }
  const newPayload = { ...payload, expiresIn: picked };
  let result;
  try {
    result = await transitionFlow(flow_id, row.version, {
      stage_to: SEND_STAGE_AWAITING_CONFIRM,
      payload: newPayload,
      terminal: false,
      set_expires_at: Math.floor(Date.now() / 1000) + SEND_FLOW_TTL_SECONDS,
    });
  } catch (err) {
    logger.error('handleConfirmExpirySelect: transitionFlow threw', {
      flow_id, error: err && err.message,
    });
    return interaction.followUp({
      content: '❌ Could not save your pick right now. Try again in a moment.',
      ephemeral: true,
    }).catch(logIgnoredDiscordErr);
  }
  if (result.result === 'conflict') {
    return interaction.editReply({
      content: 'Send was superseded — re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }
  if (result.result === 'not_found') {
    return interaction.editReply({
      content: 'This send expired — re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }
  return rerenderConfirmCard(interaction, newPayload);
}

// Self-destruct StringSelectMenu → update payload.selfDestructSeconds.
// Uses the FORM value-space helper (selfDestructSelectValueToSeconds)
// because the menu options use SELF_DESTRUCT_NO_TIMER_VALUE — distinct
// from the slash option's `'none'` value handled by
// selfDestructOptionToSeconds. The helper falls back to null for any
// unexpected value (forged interaction), which is the safe default.
async function handleConfirmSelfDestructSelect(interaction, { flow_id, row }) {
  // `?.[0]` paranoia (see expiry handler) — legitimate
  // StringSelectMenu interactions always carry the values array.
  const pickedValue = interaction.values?.[0];
  // Validate against the closed legitimate set BEFORE acknowledging.
  // Discord enforces the choice list server-side; an off-set value
  // here means a forged interaction. Silently mapping forged values
  // to null would clear a user's previously-set timer on every probe
  // — symmetric with the expiry handler's reject-and-warn behavior.
  if (!isLegitimateSelfDestructSelectValue(pickedValue)) {
    logger.warn('handleConfirmSelfDestructSelect: forged off-set self-destruct value', {
      flow_id, value: truncForLog(pickedValue),
    });
    return interaction.reply({
      content: '❌ Unrecognized self-destruct value. Re-pick from the list.',
      ephemeral: true,
    }).catch(logIgnoredDiscordErr);
  }
  await interaction.deferUpdate().catch(logIgnoredDiscordErr);
  const selfDestructSeconds = selfDestructSelectValueToSeconds(pickedValue);
  const payload = row.payload || {};
  // No-op re-pick (same value as current state) → skip the write +
  // version bump (see expiry handler for the full rationale).
  if (selfDestructSeconds === payload.selfDestructSeconds) {
    return rerenderConfirmCard(interaction, payload);
  }
  const newPayload = { ...payload, selfDestructSeconds };
  let result;
  try {
    result = await transitionFlow(flow_id, row.version, {
      stage_to: SEND_STAGE_AWAITING_CONFIRM,
      payload: newPayload,
      terminal: false,
      set_expires_at: Math.floor(Date.now() / 1000) + SEND_FLOW_TTL_SECONDS,
    });
  } catch (err) {
    logger.error('handleConfirmSelfDestructSelect: transitionFlow threw', {
      flow_id, error: err && err.message,
    });
    return interaction.followUp({
      content: '❌ Could not save your pick right now. Try again in a moment.',
      ephemeral: true,
    }).catch(logIgnoredDiscordErr);
  }
  if (result.result === 'conflict') {
    return interaction.editReply({
      content: 'Send was superseded — re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }
  if (result.result === 'not_found') {
    return interaction.editReply({
      content: 'This send expired — re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }
  return rerenderConfirmCard(interaction, newPayload);
}

// Note button → opens a modal with the current personalMessage pre-
// filled. Does NOT call transitionFlow — the flow row is untouched
// until the modal SUBMITS (handleConfirmNoteModal below). This
// asymmetry matters: clicking the button to peek at the current note
// (or even to discard it via cancel) must not bump the flow version
// and risk fencing out a concurrent picker/expiry/self-destruct
// mutation.
async function handleConfirmNoteButton(interaction, { flow_id, row }) {
  const payload = row.payload || {};
  const modal = new ModalBuilder()
    .setCustomId(CONFIRM_NOTE_MODAL_CUSTOM_ID)
    .setTitle('Personal message');
  modal.addComponents(new ActionRowBuilder().addComponents(
    new TextInputBuilder()
      .setCustomId(SEND_NOTE_MODAL_FIELD_ID)
      .setLabel('Optional note (leave blank to clear)')
      .setStyle(TextInputStyle.Paragraph)
      .setMaxLength(PERSONAL_MESSAGE_INPUT_MAX)
      .setRequired(false)
      // Pre-fill from raw to avoid double-escape on resubmit. Empty
      // fallback for legacy flow rows missing the field (3-min TTL).
      // Note: raw can contain bidi/zero-width chars (sanitize hasn't
      // run on raw); pre-filling them into the TextInput is by design
      // for round-trip correctness, and the surface is ephemeral-to-
      // self (only the invoking user sees the modal). If this ever
      // gets piped to a non-ephemeral context, sanitize the prefill.
      .setValue(payload.personalMessageRaw || '')
  ));
  return interaction.showModal(modal).catch((err) => {
    logger.warn('handleConfirmNoteButton: showModal failed', {
      flow_id, error: err && err.message,
    });
    // showModal failure leaves the button-click interaction
    // unacknowledged. Without a fallback ack the user sees Discord's
    // generic "interaction failed" toast with no remediation hint —
    // symmetric with the menu handlers' error-followUp shape.
    return interaction.reply({
      content: '❌ Could not open the note editor — try again.',
      ephemeral: true,
    }).catch(logIgnoredDiscordErr);
  });
}

// Note modal-submit → sanitize input, update payload.personalMessage.
// Modal-submit interactions get a fresh 3s ack window. We defer
// FIRST (deferUpdate) for budget headroom under slow DDB writes,
// then editReply on the deferred interaction. The deferred ack
// targets the ORIGINAL message (the confirm card) because flow-
// dispatch wires modal submits to the same message-bound
// interaction context. Post-defer error paths use followUp
// (ephemeral) — reply / update would 409 against the acked
// interaction.
async function handleConfirmNoteModal(interaction, { flow_id, row }) {
  // Defer the modal-submit ack BEFORE the DDB write — without this,
  // a slow conditional write (throttled partition) can push past
  // Discord's 3-second hard deadline, after which `update()` /
  // `reply()` both fail and the user gets an "interaction failed"
  // toast. Mirrors the menu handlers' shape.
  await interaction.deferUpdate().catch(logIgnoredDiscordErr);
  // Defensive read: `getTextInputValue` throws if the customId
  // allowlist ever drifts from SEND_NOTE_MODAL_FIELD_ID. Don't
  // silently clear the existing note — surface an ephemeral error
  // so the user knows their submit failed and their stored note is
  // unchanged. Early-return preserves the existing payload.
  let raw;
  try {
    raw = (interaction.fields.getTextInputValue(SEND_NOTE_MODAL_FIELD_ID) || '').trim();
  } catch (err) {
    logger.warn('handleConfirmNoteModal: getTextInputValue threw', {
      flow_id, error: err && err.message,
    });
    return interaction.followUp({
      content: '❌ Could not read your note input — try again. (Your existing note, if any, is unchanged.)',
      ephemeral: true,
    }).catch(logIgnoredDiscordErr);
  }
  // Cap matches the slash-entry path; both forms derive from the same
  // trimmed-and-capped raw so an Edit round-trip is idempotent.
  const rawCapped = raw ? safeCodepointSlice(raw, PERSONAL_MESSAGE_INPUT_MAX) : null;
  // `|| null`: sanitizeMessage returns '' (not null) when the input
  // strips to empty. Normalize so the lockstep below treats both
  // forms consistently. See slash-entry site for the full rationale.
  const personalMessage = rawCapped ? (sanitizeMessage(rawCapped) || null) : null;
  // Null out raw if sanitize stripped to empty (ZWSP-only / bidi-only
  // input). Keeps the button label and the modal pre-fill consistent.
  const personalMessageRaw = personalMessage ? rawCapped : null;
  const payload = row.payload || {};
  // No-op submit (same content as current state) → skip the DDB
  // write + version bump (symmetric with expiry / self-destruct
  // handlers). Compare BOTH forms because a sanitize-semantics
  // change could leave sanitized identical but raw drifted; matching
  // both is the correct invariant. Visible feedback (rerender) still
  // fires so the user knows the submit registered.
  if (personalMessage === (payload.personalMessage ?? null)
      && personalMessageRaw === (payload.personalMessageRaw ?? null)) {
    return rerenderConfirmCard(interaction, payload);
  }
  const newPayload = { ...payload, personalMessage, personalMessageRaw };
  let result;
  try {
    result = await transitionFlow(flow_id, row.version, {
      stage_to: SEND_STAGE_AWAITING_CONFIRM,
      payload: newPayload,
      terminal: false,
      set_expires_at: Math.floor(Date.now() / 1000) + SEND_FLOW_TTL_SECONDS,
    });
  } catch (err) {
    logger.error('handleConfirmNoteModal: transitionFlow threw', {
      flow_id, error: err && err.message,
    });
    // Post-deferUpdate the only safe surface for an error toast is
    // followUp (ephemeral). reply() would 409 — the interaction is
    // already acked.
    return interaction.followUp({
      content: '❌ Could not save your note right now. Try again in a moment.',
      ephemeral: true,
    }).catch(logIgnoredDiscordErr);
  }
  if (result.result === 'conflict') {
    return interaction.editReply({
      content: 'Send was superseded — re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }
  if (result.result === 'not_found') {
    return interaction.editReply({
      content: 'This send expired — re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }
  return rerenderConfirmCard(interaction, newPayload);
}

// Send button → fire executeSendPipeline. deleteFlow first as the
// dedup primitive (duplicate dispatch under future SQS at-least-once
// must not double-send). Mirrors handleRevokeSelect's ordering.
async function handleConfirmSendClick(interaction, { flow_id, row }) {
  // `deferUpdate` at the very top so the chain
  // `resolveRecipientUsers → getGuildApiKey → deleteFlow → editReply`
  // can take more than Discord's 3-second hard ack deadline without
  // surfacing as an "interaction failed" toast to the user. Cold
  // cache + 25 cache-miss `members.fetch` calls in `resolveRecipientUsers`
  // alone can chew through the budget. The .catch swallows the
  // (rare) race where Discord's gateway already acked the
  // interaction; a duplicate defer there throws InteractionAlreadyReplied
  // and the subsequent editReply still works.
  //
  // All ephemeral error-replies below switch from `interaction.reply`
  // to `interaction.followUp` (the interaction is now in the
  // deferred state and `.reply` would throw); main-message updates
  // switch from `interaction.update` to `interaction.editReply`.
  await interaction.deferUpdate().catch(logIgnoredDiscordErr);

  // Bot-kicked-between-confirm-and-Send: `interaction.guild` is null.
  // Without this guard the user sees "all recipients left the server"
  // (the all-unresolved branch's copy below) — misleading, because
  // the bot left, not the recipients. Delete the flow row so a
  // future re-invite + rerun starts fresh.
  //
  // No `expectedVersion` on this deleteFlow (unlike the happy-path /
  // empty-recipients / all-unresolved branches): if the bot has been
  // kicked, every subsequent interaction for this guild is dead
  // (Discord won't deliver further events to this bot for this guild)
  // — there's no concurrent path that could bump the version. The
  // version-gate fences picker-vs-Send and Cancel-vs-Send races; both
  // require an active gateway session that bot-kicked has terminated.
  if (!interaction.guild) {
    await deleteFlow(flow_id, {
      stage: SEND_STAGE_AWAITING_CONFIRM,
      reason: 'terminal',
    }).catch((err) => logger.warn('handleConfirmSendClick: deleteFlow on bot-kicked failed', {
      flow_id, error: err && err.message,
    }));
    // Zero side effects (no DMs sent, no API calls) — unlock retry
    // immediately after re-invite without paying the cooldown window.
    clearCooldown(interaction.user.id);
    return interaction.editReply({
      content: '❌ qURL bot is no longer in this server. Re-invite the bot and re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }

  const payload = row.payload || {};
  // Forged-interaction defense: a legitimate Send click can only land
  // when the card has at least one recipient (Send is disabled while
  // recipientIds is empty — both at confirm-card-render time and in
  // the picker re-render after an empty pick). A click with empty
  // recipientIds therefore implies a fabricated interaction. The
  // "all left the server" copy below would be misleading for this
  // path (nobody left, nobody was ever there); distinct copy + flow
  // delete keeps the dispatcher's outer catch from masking the signal.
  if (!Array.isArray(payload.recipientIds) || payload.recipientIds.length === 0) {
    // Version-gate the delete: a concurrent UserSelect could have
    // transitioned the row between the dispatcher's loadFlow (which
    // fed our stale `payload` view) and this click. If `deleted:
    // false`, the more-recent row is preserved and the user sees the
    // "card moved" recovery copy instead of a silently wiped pick.
    const deleteResult = await deleteFlow(flow_id, {
      stage: SEND_STAGE_AWAITING_CONFIRM,
      reason: 'terminal',
      expectedVersion: row.version,
    }).catch((err) => {
      logger.warn('handleConfirmSendClick: deleteFlow on empty-recipients failed', {
        flow_id, error: err && err.message,
      });
      return { deleted: false };
    });
    if (!deleteResult.deleted) {
      return interaction.followUp({
        content: 'The card moved — re-check it and click Send again.',
        ephemeral: true,
      }).catch(logIgnoredDiscordErr);
    }
    return interaction.editReply({
      content: '❌ No recipients were selected — re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }
  // Re-fetch users + resolve apiKey IN PARALLEL at click time. Both
  // are idempotent reads (rule from handleRevokeSelect: parallelize
  // ONLY with idempotent reads), so the cold-cache happy path saves
  // a DDB round-trip vs. running them sequentially. allSettled keeps
  // each failure routable to its own user-actionable copy below; a
  // rejection in one doesn't short-circuit the other.
  //
  // Both unresolved buckets (10007 + transient fetch failure) reduce
  // the delivered count, so both are surfaced to the sender via
  // followUp below — without the split, a 429 / gateway blip would
  // silently shrink the delivered set with no signal.
  const [resolveResult, apiKeyResult] = await Promise.allSettled([
    resolveRecipientUsers(interaction, payload.recipientIds),
    db.getGuildApiKey(interaction.guildId),
  ]);
  if (resolveResult.status === 'rejected') {
    logger.error('handleConfirmSendClick: resolveRecipientUsers threw', {
      flow_id, error: resolveResult.reason && resolveResult.reason.message,
    });
    // Zero side effects — unlock retry so the user can re-click Send
    // immediately after the transient blip clears.
    clearCooldown(interaction.user.id);
    return interaction.followUp({
      content: '❌ Could not look up recipients right now. Try **Send** again in a moment.',
      ephemeral: true,
    }).catch(logIgnoredDiscordErr);
  }
  const { users, unresolvedIds, transientFailureIds } = resolveResult.value;
  const partialLeftCount = unresolvedIds.length;
  const partialTransientCount = transientFailureIds.length;
  if (partialLeftCount > 0 || partialTransientCount > 0) {
    logger.info('handleConfirmSendClick: partial drop at click time', {
      flow_id, left: partialLeftCount, transient: partialTransientCount,
    });
  }
  const { valid, droppedBots } = partitionRecipients(users, interaction.user.id);
  if (valid.length === 0) {
    // Distinguish the failure mode by what was dropped. The general
    // case is "everyone left the guild between confirm and Send"
    // (10007 / transient) — but a forged Send click with payload
    // recipientIds containing only bots would resolve fine and then
    // partition would drop everything, hitting this branch with the
    // WRONG copy ("they left the server" misdirects when nobody left
    // — the recipient list was invalid). Self-send is supported so
    // a self-only recipientIds is a legitimate Send and never reaches
    // this branch. Terminal for THIS attempt either way: delete the
    // flow so a rerun claims a fresh slot.
    //
    // Version-gate the delete (same shape as the happy-path Send and
    // the empty-recipientIds branch): a concurrent UserSelect could
    // have advanced the row between the dispatcher's loadFlow and
    // this point. If `deleted: false`, the more-recent row is
    // preserved and the user sees the "card moved" recovery copy.
    const deleteResult = await deleteFlow(flow_id, {
      stage: SEND_STAGE_AWAITING_CONFIRM,
      reason: 'terminal',
      expectedVersion: row.version,
    }).catch((err) => {
      logger.warn('handleConfirmSendClick: deleteFlow on empty failed', {
        flow_id, error: err && err.message,
      });
      return { deleted: false };
    });
    if (!deleteResult.deleted) {
      return interaction.followUp({
        content: 'The card moved — re-check it and click Send again.',
        ephemeral: true,
      }).catch(logIgnoredDiscordErr);
    }
    return interaction.editReply({
      content: droppedBots > 0
        ? '❌ Invalid recipient list — re-run the command with at least one real user.'
        : '❌ Recipients are no longer reachable (all left the server). Re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }

  // apiKey was resolved in parallel with resolveRecipientUsers above.
  // Check the rejection branch before consuming the value.
  //
  // `expectedVersion: row.version` (used below on deleteFlow) fences
  // the picker-then-Send race: if a UserSelectMenu interaction landed
  // and transitioned the flow between the dispatcher's loadFlow (which
  // fed `row` here) and our deleteFlow, the version would have
  // advanced. Without the version gate, we'd deleteFlow successfully
  // and call executeSendPipeline with the STALE recipientIds captured
  // in `payload` above. `deleted: false` collapses duplicate dispatch
  // AND mid-flight picker mutation; both map to the same user
  // recovery ("the card moved under you, re-click Send").
  // `interaction.guildId` is guaranteed non-null at this point —
  // handleQurlSlashSend rejects DM invocations BEFORE the flow row
  // is created, so a row at SEND_STAGE_AWAITING_CONFIRM only ever
  // belongs to a guild interaction.
  //
  // apiKey gate runs BEFORE deleteFlow: if both guildApiKey AND
  // config.QURL_API_KEY are null (rare — key rotation removed the
  // key), the flow row stays alive and the user can re-click Send
  // within the TTL after the admin reruns `/qurl setup`. Burning the
  // row before discovering there's no key to send with would strand
  // the user on a dead card.
  if (apiKeyResult.status === 'rejected') {
    logger.error('handleConfirmSendClick: getGuildApiKey threw', {
      flow_id, error: apiKeyResult.reason && apiKeyResult.reason.message,
    });
    // Flow row stays alive; unlock retry so the user isn't stranded
    // for 30s waiting for a DDB blip to clear.
    clearCooldown(interaction.user.id);
    return interaction.followUp({
      content: '❌ Could not look up the qURL API key right now. Try **Send** again in a moment.',
      ephemeral: true,
    }).catch(logIgnoredDiscordErr);
  }
  const guildApiKey = apiKeyResult.value;
  // Check the resolved key BEFORE deleteFlow. If both guildApiKey and
  // config.QURL_API_KEY are null (rare: key rotation removed the key
  // between dispatcher pre-check and Send click), there's nothing to
  // send with — preserving the flow row lets the user retry after the
  // admin re-runs `/qurl setup` without re-invoking the slash command.
  // Cooldown clears so the user isn't stranded for the 30s window
  // while waiting on the admin.
  const apiKey = guildApiKey || config.QURL_API_KEY;
  if (!apiKey) {
    clearCooldown(interaction.user.id);
    return interaction.editReply({
      content: '❌ qURL is no longer configured for this server. Ask an admin to run `/qurl setup`.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }

  const deleteResult = await deleteFlow(flow_id, {
    stage: SEND_STAGE_AWAITING_CONFIRM,
    reason: 'terminal',
    expectedVersion: row.version,
  });
  if (!deleteResult.deleted) {
    return interaction.followUp({
      content: 'Recipients changed before Send fired — re-check the card and click Send again.',
      ephemeral: true,
    }).catch(logIgnoredDiscordErr);
  }

  // Ack with a "Preparing send…" placeholder; executeSendPipeline
  // takes over the editReply from here.
  await interaction.editReply({ content: 'Preparing send…', components: [] }).catch(logIgnoredDiscordErr);

  // Surface partial drops (members who left the guild between confirm
  // and Send, OR who failed lookup transiently) as a separate
  // ephemeral followUp BEFORE the back-half takes over the main
  // reply. Without this the user would see "Sent to N users" with no
  // signal that N != the count shown on the card. Distinct wording
  // for the two buckets — "left the server" is stable, "lookup
  // blipped" encourages a fresh rerun if they want to include the
  // missed recipients. The rerun hint names the actual subcommand
  // they invoked (resourceType drives /qurl send vs /qurl map).
  //
  // followUp (not edited into the "Preparing send…" editReply) is
  // INTENTIONAL: executeSendPipeline will rewrite editReply to
  // "Sent to N users" / failure copy as the send progresses, which
  // would clobber the partial-drop banner if it lived in the same
  // message. followUp is a separate ephemeral that persists past
  // the back-half's editReply rewrites.
  if (partialLeftCount > 0 || partialTransientCount > 0) {
    const rerunCommand = payload.resourceType === RESOURCE_TYPES.MAPS ? '/qurl map' : '/qurl send';
    const parts = [];
    if (partialLeftCount > 0) {
      parts.push(`${partialLeftCount} recipient${partialLeftCount === 1 ? '' : 's'} had left the server`);
    }
    if (partialTransientCount > 0) {
      parts.push(`${partialTransientCount} couldn't be looked up just now (rerun ${rerunCommand} to retry them)`);
    }
    await interaction.followUp({
      // "Attempting to" (not "sending to") so a downstream pipeline
      // failure doesn't leave the user reading a confident "sending"
      // message alongside a subsequent failure message.
      content: `ℹ\u{FE0F} ${parts.join('; ')} — attempting to send to the remaining ${valid.length}.`,
      ephemeral: true,
    }).catch(logIgnoredDiscordErr);
  }

  return executeSendPipeline(interaction, {
    apiKey,
    resourceType: payload.resourceType,
    attachment: payload.attachment,
    locationUrl: payload.locationUrl,
    locationName: payload.locationName,
    recipients: valid,
    expiresIn: payload.expiresIn,
    selfDestructSeconds: payload.selfDestructSeconds,
    personalMessage: payload.personalMessage,
    sendNonce: payload.sendNonce,
    recipientMode: payload.recipientMode,
  });
}

// Cancel button → version-gated deleteFlow + acknowledge.
//
// expectedVersion fences two distinct races, both serious:
//  1. Cancel vs. Send: if Send is mid-dispatch (already deleted the
//     flow), Cancel must NOT clear the cooldown — the first send is
//     fanning out DMs and the user could otherwise immediately rerun
//     and bypass the per-user cooldown window mid-send.
//  2. Cancel vs. UserSelect: a Cancel click that lands while a
//     UserSelectMenu transition is in flight would otherwise silently
//     kill the row out from under the user's pick. The version gate
//     surfaces that as the dedup-loser path (user re-clicks Cancel on
//     the new row if they still want to abort).
//
// On a successful Cancel, the cooldown is CLEARED so a user who hits
// "wrong file, retry" doesn't run into a "Please wait before sending
// again" message on their next /qurl send. The cancel-loser branch
// (deleted=false) leaves cooldown untouched — Send won the race and is
// mid-fanout; bypassing it is the abuse vector. Earlier versions used
// a 5s-residual "soften" to defend against /qurl send → Cancel → /qurl
// send spam, but the rapid-cancel-spam scenario is bounded by Discord's
// own slash-command rate limiting AND requires a deliberate human click
// loop; the UX cost of false-positive blocks on legitimate retries
// outweighed the abuse defense.
async function handleConfirmCancelClick(interaction, { flow_id, row }) {
  // Defer-ack within the 3s window. The single deleteFlow call below
  // is fast in the happy path, but a DDB blip or slow region could
  // still blow the budget. Same pattern handleConfirmSendClick uses
  // (deferUpdate at top, editReply / followUp downstream).
  await interaction.deferUpdate().catch(logIgnoredDiscordErr);
  // Targeted catch around deleteFlow mirrors handleConfirmSendClick's
  // guards on resolveRecipientUsers + getGuildApiKey. Without it, a
  // DDB throw propagates to the dispatcher's outer catch which
  // surfaces a generic "superseded" message — wrong for Cancel
  // (the user wanted to abort; the flow row may still be alive
  // and Send may still be in flight, so cooldown stays set).
  let deleted;
  try {
    ({ deleted } = await deleteFlow(flow_id, {
      stage: SEND_STAGE_AWAITING_CONFIRM,
      reason: 'terminal',
      expectedVersion: row.version,
    }));
  } catch (err) {
    logger.error('handleConfirmCancelClick: deleteFlow threw', {
      flow_id, error: err && err.message,
    });
    return interaction.followUp({
      content: '❌ Could not cancel right now — try clicking **Cancel** again in a moment.',
      ephemeral: true,
    }).catch(logIgnoredDiscordErr);
  }
  if (!deleted) {
    // Dedup-loser: either Send won the race (deletedFlow already, fan-out
    // in progress) or a UserSelect transition advanced the row version.
    // Distinct branches share the same recovery (re-check the card),
    // but the wording avoids implying the flow was "processed" on the
    // picker-race path where nothing actually committed.
    return interaction.followUp({
      content: 'The card moved — re-check it.',
      ephemeral: true,
    }).catch(logIgnoredDiscordErr);
  }
  clearCooldown(interaction.user.id);
  return interaction.editReply({
    content: 'Send cancelled.',
    components: [],
  }).catch(logIgnoredDiscordErr);
}

// Pure rendering / wording logic lives in `./revoke-render` so the
// e2e smoke test can require it without pulling in `discord.js`.
const {
  REVOKE_TRUNC_LIMIT,
  REVOKE_CONTENT_SAFE_MAX,
  buildRevokeHeader,
  renderRevokeContent,
} = require('./revoke-render');

// Builds the editReply payload from a `renderRevokeMsg` result. When
// the rendered names list overflowed the Discord content cap,
// `attachmentText` is populated → wrap in a `revoked-users.txt`
// attachment and drop the Show Recipients button (the file IS the full list).
function revokeReplyPayload(rendered) {
  const payload = { content: rendered.content };
  payload.components = rendered.row ? [rendered.row] : [];
  if (rendered.attachmentText) {
    payload.files = [new AttachmentBuilder(Buffer.from(rendered.attachmentText, 'utf8'), { name: 'revoked-users.txt' })];
  } else {
    payload.files = [];
  }
  return payload;
}

// Wraps `renderRevokeContent` (pure data) and adds the discord.js
// ActionRowBuilder/ButtonBuilder for the Show Recipients / Hide
// Recipients toggle on the post-revoke "Revoked for: ..." list.
// All wording assertions live against `renderRevokeContent` directly
// (see `apps/discord/src/revoke-render.js` + the e2e smoke).
function renderRevokeMsg(sendId, names, total, showAll, success = names.length) {
  const data = renderRevokeContent({ names, total, showAll, success });
  const row = data.needsExpand
    ? new ActionRowBuilder().addComponents(
      new ButtonBuilder()
        .setCustomId(`qurl_revoke_expand_${sendId}`)
        .setLabel(showAll ? 'Hide Recipients' : 'Show Recipients')
        .setStyle(ButtonStyle.Secondary),
    )
    : null;
  return { ...data, row };
}

// Builds the post-send confirmation body. When the full inline render
// would exceed Discord's 2000-char content cap, falls back to a
// `recipients.txt` attachment; "(see attached)" is appended only to
// lines that were actually truncated.
function renderSendConfirm({
  delivered, expiresIn, selfDestructSeconds,
  failedNamesPlain = [], successNames = [], showAll = false,
}) {
  const header = `Sent to ${delivered} user${delivered !== 1 ? 's' : ''} | Expires: ${expiresIn} | ${formatSelfDestructSegment(selfDestructSeconds)}`;
  const escapedFailed = failedNamesPlain.map(escapeDiscordMarkdown);
  const escapedSuccess = successNames.map(escapeDiscordMarkdown);

  const failedCount = failedNamesPlain.length;
  const fullFailedLine = failedCount > 0 ? `\n${failedCount} could not be reached: ${escapedFailed.join(', ')}` : '';
  const fullRecipientsLine = successNames.length > 0 ? `\nRecipients: ${escapedSuccess.join(', ')}` : '';
  const fullFits = (header + fullFailedLine + fullRecipientsLine).length <= REVOKE_CONTENT_SAFE_MAX;

  // Truncated preview line for one of the two lists; emits "+N more
  // (see attached)" only when the list itself overflowed.
  const truncatedLine = (escaped, plain, prefix) => {
    if (plain.length <= REVOKE_TRUNC_LIMIT) return prefix + escaped.join(', ');
    const preview = escaped.slice(0, REVOKE_TRUNC_LIMIT).join(', ');
    return `${prefix}${preview} +${plain.length - REVOKE_TRUNC_LIMIT} more (see attached)`;
  };

  if (!fullFits) {
    let msg = header;
    if (failedCount > 0) msg += truncatedLine(escapedFailed, failedNamesPlain, `\n${failedCount} could not be reached: `);
    if (successNames.length > 0) msg += truncatedLine(escapedSuccess, successNames, '\nRecipients: ');
    let attachmentText = '';
    if (successNames.length > 0) {
      attachmentText += `DELIVERED (${successNames.length}):\n${successNames.join('\n')}`;
    }
    if (failedCount > 0) {
      if (attachmentText) attachmentText += '\n\n';
      attachmentText += `NOT DELIVERED (${failedCount}):\n${failedNamesPlain.join('\n')}`;
    }
    return { content: msg, attachmentText, needsExpand: false };
  }

  let msg = header;
  if (failedCount > 0) msg += fullFailedLine;
  if (successNames.length > 0) {
    if (showAll || escapedSuccess.length <= REVOKE_TRUNC_LIMIT) {
      msg += fullRecipientsLine;
    } else {
      msg += `\nRecipients: ${escapedSuccess.slice(0, REVOKE_TRUNC_LIMIT).join(', ')} +${escapedSuccess.length - REVOKE_TRUNC_LIMIT} more`;
    }
  }
  return { content: msg, attachmentText: null, needsExpand: successNames.length > REVOKE_TRUNC_LIMIT };
}

// Defaulted senderAlias is defense-in-depth — production callers
// always pass resolveSenderAlias(interaction), which has its own
// DISPLAY_NAME_FALLBACK, so a forgotten 4th arg still renders
// gracefully on the recipient side.
async function revokeAllLinks(sendId, senderDiscordId, apiKey, senderAlias = DISPLAY_NAME_FALLBACK) {
  // Items carry dm_channel_id / dm_message_id / dm_status so the post-
  // revoke step can edit each strict-success recipient's DM in place.
  // Legacy rows predating that wire-up have the refs unset — the edit
  // step skips them.
  const items = await db.getSendItems(sendId, senderDiscordId);

  // deleteLink deletes the whole resource; one DELETE per unique
  // resource_id, fan result out to every recipient sharing it.
  // Required because mintLinksInBatches packs up to TOKENS_PER_RESOURCE
  // recipients per resource, so the same resource_id is shared.
  const byResource = new Map();
  for (const item of items) {
    const list = byResource.get(item.resource_id) || [];
    list.push(item.recipient_discord_id);
    byResource.set(item.resource_id, list);
  }
  const resourceEntries = [...byResource.entries()];
  const totalUsers = new Set(items.map(it => it.recipient_discord_id)).size;

  // Record the user's revocation intent before side-effecting DELETEs.
  // If this write fails, no qURL resource has been deleted yet, so callers
  // can safely treat the revoke as failed and leave Add Recipients available.
  // Do not emit revoke_success/revoke_failed before this point: those audit
  // events describe qURL DELETE outcomes, and no DELETE has happened yet.
  // Mark regardless of per-link success: partial failures surface in the
  // reply ("Revoked X/Y"), and re-picking the same send would not help.
  await db.markSendRevoked(sendId, senderDiscordId);

  const successUserIds = [];
  const failureUserIds = [];

  const results = await batchSettled(resourceEntries, async ([resourceId]) => {
    await deleteLink(resourceId, apiKey);
    return resourceId;
  }, 5);

  // User-centric: strict-success = recipient whose every link was
  // revoked (in success but not in failure). Mixed-outcome users
  // (some links succeeded, some failed) count as failure — better to
  // tell the operator "alice is partial" via failure than misleadingly
  // claim full success.
  const seenSuccess = new Set();
  const seenFailure = new Set();
  for (let i = 0; i < results.length; i++) {
    const [resourceId, recipientIds] = resourceEntries[i];
    if (results[i].status === 'fulfilled') {
      for (const id of recipientIds) seenSuccess.add(id);
    } else {
      for (const id of recipientIds) seenFailure.add(id);
      logger.error('Failed to revoke QURL', { resource_id: resourceId, error: results[i].reason?.message });
    }
  }
  // Strict success = revoked AND not in any failure bucket.
  for (const id of seenSuccess) {
    if (!seenFailure.has(id)) successUserIds.push(id);
  }
  for (const id of seenFailure) failureUserIds.push(id);

  const success = successUserIds.length;
  const total = totalUsers;
  // Audit metric is per-resource (DELETE call), not per-recipient.
  const auditTotal = byResource.size;
  const auditSuccess = results.filter(r => r.status === 'fulfilled').length;

  // Emit audit after DELETE attempts so the tally reflects actual qURL API
  // outcomes. The revocation-intent write happened above, before any
  // destructive side effect.
  if (total > 0) {
    const event = success > 0 ? AUDIT_EVENTS.REVOKE_SUCCESS : AUDIT_EVENTS.REVOKE_FAILED;
    logger.audit(event, { send_id: sendId, success: auditSuccess, total: auditTotal });
  }

  // Top-level `success/total` are per-resource (matches the audit
  // event); per-recipient counts surface in nested `users`.
  logger.info('Revoked send', {
    sendId,
    success: auditSuccess,
    total: auditTotal,
    users: { success, total },
  });

  // Edit each strict-success recipient's DM to "Alice closed the door"
  // so they see immediately that the link is dead rather than tapping a
  // Step Through button that now 404s. Per-recipient (one DM per
  // recipient, even if multiple resources fanned out to them) — keep
  // the first VALID row per recipient_discord_id. Invalid rows fall
  // through without setting, so editTargets.has() stays false until a
  // valid row lands, which then wins the slot.
  //
  // INVARIANT(one-dm-per-recipient): "first valid wins" is safe
  // because each recipient has at most one DM channel (the bot opens
  // one DM thread per user via sendDM's POST /users/@me/channels).
  // Multi-resource fan-out reuses the same dm_channel_id /
  // dm_message_id pair across rows for that recipient, so picking
  // the first one isn't lossy.
  //
  // INVARIANT-BREAKER: if a future change ever sends a recipient two
  // SEPARATE DMs for one send_id (e.g., multi-message dispatch when
  // the embed/component count exceeds Discord's limits), the second
  // DM becomes silently un-editable. Grep for `INVARIANT-BREAKER`
  // from that PR to find this site, and flip to a per-message-id
  // Map keyed by `(recipient_discord_id, dm_message_id)`.
  //
  // ORDERING: DELETEs ran above; the edit fires AFTER they settle.
  // Reversing the order would create a window where the recipient sees
  // "closed the door" while the link still resolves until DELETE
  // completes. With DELETE first, the worst case is a brief window where
  // the live button now 404s — the same window that existed before this
  // feature, minus the post-revoke rewrite.
  //
  // Skips:
  //   - recipients not in successUserIds (their revoke didn't fully
  //     succeed — the link may have been already opened, a transient
  //     qURL API error during DELETE, or a mixed-outcome recipient
  //     with some links failed. Either way the door isn't fully
  //     closed for them, so the "closed the door" copy would be
  //     misleading)
  //   - rows with dm_status !== 'sent' (DM never made it; no message
  //     exists to edit)
  //   - rows lacking dm_channel_id / dm_message_id (legacy, pre-
  //     ref-capture rows)
  //
  // Errors are swallowed (logged inside editDM at info/warn) — a 404 /
  // 403 / unknown-message is operational, not a bug, and must not
  // skew the revoke success counts the caller reports to the operator.
  if (success > 0) {
    const successSet = new Set(successUserIds);
    const editTargets = new Map(); // recipient_id → {channelId, messageId}
    for (const it of items) {
      if (!successSet.has(it.recipient_discord_id)) continue;
      if (editTargets.has(it.recipient_discord_id)) continue;
      if (it.dm_status !== DM_STATUS.SENT) continue;
      if (!it.dm_channel_id || !it.dm_message_id) continue;
      editTargets.set(it.recipient_discord_id, {
        channelId: it.dm_channel_id, messageId: it.dm_message_id,
      });
    }
    if (editTargets.size === 0) {
      // Surface the silent-skip path so a developer running locally
      // (where DM refs are intentionally not persisted on revoke —
      // see #365) can tell the edit is being skipped on purpose
      // rather than chasing a phantom bug. Debug level so it doesn't
      // surface in prod logs by default.
      logger.debug('Revoke succeeded but no editable DM targets', {
        sendId, revoke_success: success,
      });
    } else {
      const editPayload = buildRevokedDMPayload({ senderAlias });
      // Same fan-out width as the DELETE batch above. Discord's per-
      // channel rate-limit buckets are unique per recipient (each DM
      // channel is its own bucket), so 5 concurrent PATCHes don't share
      // a bucket; the bound exists to limit global rate-limit pressure
      // and audit-log burst, not per-channel contention.
      const entries = [...editTargets.entries()];
      // editDM swallows its own exceptions and returns { ok, expected }.
      // Re-throw on `!ok` so batchSettled buckets failures uniformly;
      // pass through res.expected so the rolled-up log can distinguish
      // operational outcomes (recipient deleted DM / blocked bot) from
      // surprises that warrant oncall attention.
      const editResults = await batchSettled(entries, async ([, refs]) => {
        const res = await editDM(refs.channelId, refs.messageId, editPayload);
        if (!res.ok) {
          const e = new Error('editDM returned not-ok');
          e.expected = res.expected;
          throw e;
        }
        return res;
      }, 5);
      let edited = 0;
      let expectedFailures = 0;
      let failed = 0;
      for (const r of editResults) {
        if (r.status === 'fulfilled') edited++;
        else if (r.reason?.expected === true) expectedFailures++;
        else failed++;
      }
      logger.info('Edited DMs after revoke', {
        sendId,
        attempted: editTargets.size,
        edited,
        // Recipient-side operational outcomes (DM deleted, bot blocked,
        // etc.) — already logged at info inside editDM. Split out here
        // so the rolled-up failure count in CloudWatch isn't poisoned
        // by user-side state changes.
        expectedFailures,
        failed,
      });
    }
  }

  // failureUserIds is computed but not yet rendered — the "Note:
  // already-opened links cannot be revoked" disclaimer covers the
  // common cause. Returned for callers that want to surface partial-
  // failure detail (e.g., a future "Failed for: …" line or follow-up
  // alert when count is large).
  return { success, total, successUserIds, failureUserIds };
}

// Time-based sweep every 60s (was 5min). With high user counts the Map can
// creep between the size-based 10k-threshold eviction, so a more aggressive
// proactive sweep keeps steady-state memory tight. An entry is safe to drop
// only once it's older than the SAME window its is-on-cooldown gate uses —
// otherwise the sweep would evict a still-active entry and silently end the
// cooldown early.
//
// detectCooldowns rides the SAME interval but is swept against
// QURL_DETECT_COOLDOWN_MS (the window isOnDetectCooldown gates on), NOT the
// send window — these can differ (the round-4 decoupling exists precisely so
// detect can be a STRICTER, longer window than send). Sweeping detect at the
// send window would truncate a longer detect cooldown back to the send
// window, defeating the decoupling.
// Named (not an inline arrow) so the windows-differ regression — a detect
// entry swept at the SEND window when DETECT > SEND — is directly testable
// via the _test export, without driving the module-level setInterval.
function sweepCooldowns() {
  const now = Date.now();
  for (const [k, v] of sendCooldowns) {
    if (now - v > config.QURL_SEND_COOLDOWN_MS) sendCooldowns.delete(k);
  }
  for (const [k, v] of detectCooldowns) {
    if (now - v > config.QURL_DETECT_COOLDOWN_MS) detectCooldowns.delete(k);
  }
}
setInterval(sweepCooldowns, 60 * 1000).unref();

// Command definitions
const commands = [
  {
    data: new SlashCommandBuilder()
      .setName('link')
      .setDescription('Link your GitHub account to receive Contributor role when PRs are merged'),
    async execute(interaction) {
      const discordId = interaction.user.id;

      // Check if already linked
      const existing = await db.getLinkByDiscord(discordId);

      // Generate state and create pending link. State is HMAC-bound to the
      // discord user ID so the OAuth callback can verify cross-user replay
      // didn't happen even if the random nonce were somehow leaked.
      const state = generateState(discordId);
      await db.createPendingLink(state, discordId);

      const authUrl = `${config.BASE_URL}/auth/github?state=${state}`;

      const embed = new EmbedBuilder()
        .setColor(COLORS.PRIMARY)
        .setTitle('🔗 Link Your GitHub Account')
        .setDescription(
          existing
            ? `You're currently linked to **@${existing.github_username}**.\n\nClick the button below to link a different account or re-verify.`
            : 'Click the button below to verify your GitHub identity.\n\n' +
              'Once linked, you\'ll automatically receive the **@Contributor** role when your PRs to OpenNHP repos are merged!'
        )
        .addFields({
          name: '🔒 Privacy',
          value: 'We only request permission to read your public profile (username). We cannot access your repositories or private information.',
        })
        .setFooter({ text: `Link expires in ${config.PENDING_LINK_EXPIRY_MINUTES} minutes` });

      const row = new ActionRowBuilder().addComponents(
        new ButtonBuilder()
          .setLabel(existing ? '🔄 Re-link GitHub' : '🔗 Link GitHub Account')
          .setStyle(ButtonStyle.Link)
          .setURL(authUrl)
      );

      await interaction.reply({
        embeds: [embed],
        components: [row],
        ephemeral: true,
      });

      logger.info('User initiated /link', { discordId, relink: !!existing });
    },
  },
  {
    data: new SlashCommandBuilder()
      .setName('unlink')
      .setDescription('Unlink your GitHub account'),
    async execute(interaction) {
      const discordId = interaction.user.id;

      const existing = await db.getLinkByDiscord(discordId);
      if (!existing) {
        return interaction.reply({
          content: 'You don\'t have a GitHub account linked.',
          ephemeral: true,
        });
      }

      // Confirmation prompt
      const embed = new EmbedBuilder()
        .setColor(COLORS.ERROR)
        .setTitle('⚠️ Confirm Unlink')
        .setDescription(
          `Are you sure you want to unlink your GitHub account **@${existing.github_username}**?\n\n` +
          'You will no longer automatically receive the @Contributor role for future PRs.'
        );

      // Nonce the customIds so two concurrent /unlink flows can't have
      // their collectors consume each other's button clicks.
      const nonce = crypto.randomBytes(8).toString('hex');
      const row = new ActionRowBuilder().addComponents(
        new ButtonBuilder()
          .setCustomId(`unlink_confirm_${nonce}`)
          .setLabel('Yes, Unlink')
          .setStyle(ButtonStyle.Danger),
        new ButtonBuilder()
          .setCustomId(`unlink_cancel_${nonce}`)
          .setLabel('Cancel')
          .setStyle(ButtonStyle.Secondary)
      );

      const response = await interaction.reply({
        embeds: [embed],
        components: [row],
        ephemeral: true,
      });

      try {
        const buttonInteraction = await response.awaitMessageComponent({
          componentType: ComponentType.Button,
          filter: (i) => i.user.id === interaction.user.id && i.customId.endsWith(`_${nonce}`),
          time: TIMEOUTS.BUTTON_INTERACTION,
        });

        if (buttonInteraction.customId === `unlink_confirm_${nonce}`) {
          await db.deleteLink(discordId);
          await buttonInteraction.update({
            content: `✓ Unlinked from GitHub **@${existing.github_username}**.\n\nYou can link a new account anytime with \`/link\`.`,
            embeds: [],
            components: [],
          });
          logger.info('User unlinked', { discordId, github: existing.github_username });
        } else {
          await buttonInteraction.update({
            content: 'Unlink cancelled. Your GitHub account is still linked.',
            embeds: [],
            components: [],
          });
        }
      } catch {
        await interaction.editReply({
          content: 'Confirmation timed out. Your GitHub account is still linked.',
          embeds: [],
          components: [],
        });
      }
    },
  },
  {
    data: new SlashCommandBuilder()
      .setName('whois')
      .setDescription('Check GitHub link for a user')
      .addUserOption(option =>
        option
          .setName('user')
          .setDescription('The Discord user to check (leave empty for yourself)')
          .setRequired(false)
      ),
    async execute(interaction) {
      const targetUser = interaction.options.getUser('user') || interaction.user;
      const link = await db.getLinkByDiscord(targetUser.id);

      if (link) {
        const contributions = await db.getContributions(targetUser.id);
        const badges = await db.getBadges(targetUser.id);
        const streak = await db.getStreak(targetUser.id);

        const embed = new EmbedBuilder()
          .setColor(COLORS.SUCCESS)
          .setTitle(`GitHub Link for ${targetUser.username}`)
          .addFields(
            { name: 'GitHub', value: `[@${link.github_username}](https://github.com/${link.github_username})`, inline: true },
            { name: 'Linked Since', value: new Date(link.linked_at).toLocaleDateString(), inline: true },
            { name: 'PRs Merged', value: `${contributions.length}`, inline: true }
          );

        // Add badges
        if (badges.length > 0) {
          const badgeDisplay = badges
            .map(b => {
              const info = db.BADGE_INFO[b.badge_type];
              return info ? `${info.emoji} ${info.name}` : b.badge_type;
            })
            .join(' • ');
          embed.addFields({ name: '🏅 Badges', value: badgeDisplay });
        }

        // Add streak (monthly tracking)
        if (streak && streak.current_streak > 0) {
          embed.addFields({
            name: '🔥 Streak',
            value: `${streak.current_streak} month${streak.current_streak > 1 ? 's' : ''} (Best: ${streak.longest_streak})`,
            inline: true,
          });
        }

        // Add recent contributions
        if (contributions.length > 0) {
          const recent = contributions.slice(0, 3)
            .map(c => `• ${c.repo} #${c.pr_number}`)
            .join('\n');
          embed.addFields({ name: 'Recent Contributions', value: recent });
        }

        const row = new ActionRowBuilder().addComponents(
          new ButtonBuilder()
            .setLabel('View on GitHub')
            .setStyle(ButtonStyle.Link)
            .setURL(`https://github.com/${link.github_username}`)
        );

        await interaction.reply({ embeds: [embed], components: [row], ephemeral: true });
      } else {
        await interaction.reply({
          content: targetUser.id === interaction.user.id
            ? 'You haven\'t linked your GitHub account yet. Use `/link` to get started!'
            : `${targetUser.username} hasn't linked their GitHub account.`,
          ephemeral: true,
        });
      }
    },
  },
  {
    data: new SlashCommandBuilder()
      .setName('contributions')
      .setDescription('View your contribution history')
      .addUserOption(option =>
        option
          .setName('user')
          .setDescription('The user to check (leave empty for yourself)')
          .setRequired(false)
      ),
    async execute(interaction) {
      const targetUser = interaction.options.getUser('user') || interaction.user;
      const contributions = await db.getContributions(targetUser.id);

      if (contributions.length === 0) {
        return interaction.reply({
          content: targetUser.id === interaction.user.id
            ? 'You don\'t have any recorded contributions yet. Link your GitHub with `/link` and merge a PR!'
            : `${targetUser.username} doesn't have any recorded contributions.`,
          ephemeral: true,
        });
      }

      // Group by repo
      const byRepo = {};
      for (const c of contributions) {
        if (!byRepo[c.repo]) byRepo[c.repo] = [];
        byRepo[c.repo].push(c);
      }

      const embed = new EmbedBuilder()
        .setColor(COLORS.PURPLE)
        .setTitle(`📊 Contributions by ${targetUser.username}`)
        .setDescription(`**${contributions.length}** PRs merged across **${Object.keys(byRepo).length}** repos`)
        .setTimestamp();

      for (const [repo, prs] of Object.entries(byRepo)) {
        const prList = prs.slice(0, 5)
          .map(p => `• #${p.pr_number}${p.pr_title ? `: ${p.pr_title.substring(0, 40)}${p.pr_title.length > 40 ? '...' : ''}` : ''}`)
          .join('\n');
        embed.addFields({
          name: `${repo} (${prs.length})`,
          value: prList + (prs.length > 5 ? `\n... and ${prs.length - 5} more` : ''),
        });
      }

      await interaction.reply({ embeds: [embed], ephemeral: true });
    },
  },
  {
    data: new SlashCommandBuilder()
      .setName('stats')
      .setDescription('Show bot statistics'),
    async execute(interaction) {
      const stats = await db.getStats();

      const embed = new EmbedBuilder()
        .setColor(COLORS.PURPLE)
        .setTitle('📊 OpenNHP Bot Stats')
        .addFields(
          { name: 'Linked Users', value: `${stats.linkedUsers}`, inline: true },
          { name: 'Total PRs', value: `${stats.totalContributions}`, inline: true },
          { name: 'Contributors', value: `${stats.uniqueContributors}`, inline: true }
        );

      if (stats.byRepo.length > 0) {
        const repoList = stats.byRepo
          .slice(0, 5)
          .map(r => `• ${r.repo}: ${r.count} PRs`)
          .join('\n');
        embed.addFields({ name: 'Top Repositories', value: repoList });
      }

      // Add leaderboard
      const topContributors = await db.getTopContributors(5);
      if (topContributors.length > 0) {
        const leaderboard = topContributors
          .map((c, i) => {
            const medal = i === 0 ? '🥇' : i === 1 ? '🥈' : i === 2 ? '🥉' : `${i + 1}.`;
            return `${medal} <@${c.discord_id}>: ${c.count} PRs`;
          })
          .join('\n');
        embed.addFields({ name: '🏆 Top Contributors', value: leaderboard });
      }

      embed.setTimestamp();

      await interaction.reply({ embeds: [embed], ephemeral: true });
    },
  },
  {
    data: new SlashCommandBuilder()
      .setName('leaderboard')
      .setDescription('Show contribution leaderboard'),
    async execute(interaction) {
      const topContributors = await db.getTopContributors(10);

      if (topContributors.length === 0) {
        return interaction.reply({
          content: 'No contributions recorded yet!',
          ephemeral: true,
        });
      }

      const embed = new EmbedBuilder()
        .setColor(COLORS.GOLD)
        .setTitle('🏆 Contribution Leaderboard')
        .setDescription(
          topContributors
            .map((c, i) => {
              const medal = i === 0 ? '🥇' : i === 1 ? '🥈' : i === 2 ? '🥉' : `**${i + 1}.**`;
              return `${medal} <@${c.discord_id}> — **${c.count}** PRs`;
            })
            .join('\n')
        )
        .setTimestamp();

      await interaction.reply({ embeds: [embed] });
    },
  },
  {
    data: new SlashCommandBuilder()
      .setName('forcelink')
      .setDescription('(Admin) Force link a Discord user to a GitHub account')
      .addUserOption(option =>
        option
          .setName('user')
          .setDescription('Discord user to link')
          .setRequired(true)
      )
      .addStringOption(option =>
        option
          .setName('github')
          .setDescription('GitHub username (without @)')
          .setRequired(true)
      )
      .setDefaultMemberPermissions(PermissionFlagsBits.Administrator),
    async execute(interaction) {
      if (!await requireAdmin(interaction)) return;

      const targetUser = interaction.options.getUser('user');
      const githubUsername = interaction.options.getString('github').replace(/^@+/, '');

      // Same validation /bulklink uses — reject anything that isn't a valid
      // GitHub login. A malformed string in guild_links would later be
      // reflected into embeds and interpolated into search queries.
      if (!/^[a-zA-Z0-9-]{1,39}$/.test(githubUsername)) {
        return interaction.reply({
          content: `❌ Invalid GitHub username format: \`${githubUsername}\`. Must be 1-39 chars, alphanumerics + hyphen only.`,
          ephemeral: true,
        });
      }

      const existingLink = await db.getLinkByGithub(githubUsername);
      if (existingLink && existingLink.discord_id !== targetUser.id) {
        return interaction.reply({
          content: `⚠️ GitHub **@${githubUsername}** is already linked to <@${existingLink.discord_id}>. Unlink them first.`,
          ephemeral: true,
        });
      }

      await db.forceLink(targetUser.id, githubUsername);

      await interaction.reply({
        content: `✓ Linked <@${targetUser.id}> to GitHub **@${githubUsername}**`,
        ephemeral: true,
      });

      logger.info('Admin force-linked user', {
        admin: interaction.user.id,
        target: targetUser.id,
        github: githubUsername,
      });
    },
  },
  {
    data: new SlashCommandBuilder()
      .setName('bulklink')
      .setDescription('(Admin) Bulk link users from a list')
      .addStringOption(option =>
        option
          .setName('mappings')
          .setDescription('Comma-separated discord_id:github pairs (e.g., 123:user1,456:user2)')
          .setRequired(true)
      )
      .setDefaultMemberPermissions(PermissionFlagsBits.Administrator),
    async execute(interaction) {
      if (!await requireAdmin(interaction)) return;

      const mappings = interaction.options.getString('mappings');
      const pairs = mappings.split(',').map(s => s.trim());

      let success = 0;
      let failed = 0;
      const errors = [];

      for (const pair of pairs) {
        const [discordId, github] = pair.split(':').map(s => s.trim());
        if (!discordId || !github) {
          failed++;
          errors.push(`Invalid format: "${pair}"`);
          continue;
        }
        if (!/^\d{17,20}$/.test(discordId)) {
          failed++;
          errors.push(`Invalid Discord ID: "${discordId}"`);
          continue;
        }
        // GitHub username format: letters/digits/hyphens, can't start/end with
        // hyphen, no consecutive hyphens, 1-39 chars.
        if (!/^[a-zA-Z0-9](?:[a-zA-Z0-9]|-(?=[a-zA-Z0-9])){0,38}$/.test(github)) {
          failed++;
          errors.push(`Invalid GitHub username: "${github}"`);
          continue;
        }

        try {
          const existing = await db.getLinkByGithub(github);
          if (existing && existing.discord_id !== discordId) {
            failed++;
            errors.push(`@${github} already linked to another user`);
            continue;
          }

          await db.forceLink(discordId, github);
          success++;
        } catch (error) {
          failed++;
          errors.push(`Error linking ${discordId}: ${error.message}`);
        }
      }

      const embed = new EmbedBuilder()
        .setColor(failed === 0 ? COLORS.SUCCESS : COLORS.WARNING)
        .setTitle('📦 Bulk Link Results')
        .addFields(
          { name: '✓ Success', value: `${success}`, inline: true },
          { name: '✗ Failed', value: `${failed}`, inline: true }
        );

      if (errors.length > 0) {
        embed.addFields({
          name: 'Errors',
          value: errors.slice(0, 10).join('\n') + (errors.length > 10 ? `\n... and ${errors.length - 10} more` : ''),
        });
      }

      await interaction.reply({ embeds: [embed], ephemeral: true });

      logger.info('Admin bulk-linked users', {
        admin: interaction.user.id,
        success,
        failed,
      });
    },
  },
  {
    data: new SlashCommandBuilder()
      .setName('backfill-milestones')
      .setDescription('(Admin) Backfill star milestones for a repo that already has stars')
      .addStringOption(option =>
        option
          .setName('repo')
          .setDescription('Full repo name (e.g., OpenNHP/opennhp)')
          .setRequired(true)
      )
      .addIntegerOption(option =>
        option
          .setName('stars')
          .setDescription('Current star count')
          .setRequired(true)
      )
      .setDefaultMemberPermissions(PermissionFlagsBits.Administrator),
    async execute(interaction) {
      if (!await requireAdmin(interaction)) return;

      const repo = interaction.options.getString('repo');
      if (!/^[\w.-]+\/[\w.-]+$/.test(repo)) {
        return interaction.reply({ content: 'Invalid repo format. Use `owner/repo` (e.g., `OpenNHP/opennhp`).', ephemeral: true });
      }
      const stars = interaction.options.getInteger('stars');

      let backfilled = 0;
      let skipped = 0;

      for (const milestone of config.STAR_MILESTONES) {
        if (stars >= milestone) {
          if (!(await db.hasMilestoneBeenAnnounced('stars', milestone, repo))) {
            if (await db.recordMilestone('stars', milestone, repo)) {
              backfilled++;
            }
          } else {
            skipped++;
          }
        }
      }

      const embed = new EmbedBuilder()
        .setColor(COLORS.SUCCESS)
        .setTitle('✓ Milestones Backfilled')
        .setDescription(`Backfilled milestones for **${repo}** (${stars} stars)`)
        .addFields(
          { name: 'Backfilled', value: `${backfilled}`, inline: true },
          { name: 'Already Recorded', value: `${skipped}`, inline: true }
        );

      await interaction.reply({ embeds: [embed], ephemeral: true });

      logger.info('Admin backfilled milestones', {
        admin: interaction.user.id,
        repo,
        stars,
        backfilled,
      });
    },
  },
  {
    data: new SlashCommandBuilder()
      .setName('unlinked')
      .setDescription('(Admin) Show contributors who haven\'t linked their GitHub')
      .setDefaultMemberPermissions(PermissionFlagsBits.Administrator),
    async execute(interaction) {
      if (!await requireAdmin(interaction)) return;

      await interaction.deferReply({ ephemeral: true });

      try {
        const guild = interaction.guild;
        const contributorRole = guild.roles.cache.find(r => r.name === config.CONTRIBUTOR_ROLE_NAME);

        if (!contributorRole) {
          return interaction.editReply({
            content: `❌ Could not find role "${config.CONTRIBUTOR_ROLE_NAME}"`,
          });
        }

        // `guild.members.fetch()` (no args) crashes in http-only worker
        // mode because it relies on a gateway shard; route through the
        // REST prewarm helper instead. After it resolves, the cache is
        // the authoritative member set.
        await prewarmGuildMembersCache(guild, { command: '/unlinked' });
        // Prewarm swallows REST failures (correct for /qurl send
        // degraded mode). For an admin reporting command, an empty
        // OR partial cache is pathological — reporting "all linked"
        // against an incomplete member set is a silent false positive.
        // Mid-pagination failures (e.g. 429 on page 6 of 12) leave a
        // non-empty cache that's still missing members. Compare against
        // the expected count with `UNLINKED_CACHE_COMPLETENESS_THRESHOLD`
        // tolerance to allow approximate-count drift but catch
        // substantive shortfalls.
        //
        // Edge case: when both `memberCount` AND
        // `approximateMemberCount` are absent (rare in practice — the
        // guild loaded via REST `?with_counts=true` populates the
        // latter), this falls back to a `size === 0` check only.
        // Best-effort under that condition; a guild that fails
        // mid-pagination with no count metadata available will
        // currently still surface "all linked" if cache is non-empty.
        const expectedMembers = effectiveGuildMemberCount(guild);
        const cacheSize = guild.members.cache?.size ?? 0;
        // `cacheSize === 0` is a sound proxy for "prewarm produced
        // nothing" because `GET /guilds/{id}/members` includes the bot
        // itself — a real guild with the bot present can never
        // legitimately return zero members. The check defends against
        // degraded-API silence, not against zero-member guilds.
        const looksIncomplete = cacheSize === 0
          || (expectedMembers != null && cacheSize < expectedMembers * UNLINKED_CACHE_COMPLETENESS_THRESHOLD);
        if (looksIncomplete) {
          // Debug-log the trip so dashboards can surface false-positive
          // patterns (e.g. `approximateMemberCount` over-reporting on
          // guilds with high churn → healthy run flagged degraded).
          logger.debug('unlinked surfaced degraded-cache message', {
            command: '/unlinked', guild_id: guild.id,
            cache_size: cacheSize, expected_members: expectedMembers,
          });
          return interaction.editReply({
            content: '⚠️ Could not load complete member list (Discord API may be degraded). Please retry.',
          });
        }
        const contributors = guild.members.cache.filter(
          (m) => m.roles.cache.has(contributorRole.id),
        );

        // Check which ones are not linked (single bulk query, not N+1)
        const linkedIds = await db.getLinkedDiscordIds();
        const unlinked = [];
        for (const [id, member] of contributors) {
          if (!linkedIds.has(id)) unlinked.push(member);
        }

        if (unlinked.length === 0) {
          return interaction.editReply({
            content: '✓ All contributors have linked their GitHub accounts!',
          });
        }

        const embed = new EmbedBuilder()
          .setColor(COLORS.ERROR)
          .setTitle('⚠️ Unlinked Contributors')
          .setDescription(
            `${unlinked.length} contributor(s) have the @${config.CONTRIBUTOR_ROLE_NAME} role but haven't linked their GitHub:\n\n` +
            unlinked.slice(0, 20).map(m => `• <@${m.id}> (${m.user.tag})`).join('\n') +
            (unlinked.length > 20 ? `\n... and ${unlinked.length - 20} more` : '')
          )
          .setFooter({ text: 'Use /forcelink to manually link these users' });

        await interaction.editReply({ embeds: [embed] });
      } catch (error) {
        logger.error('Error in /unlinked', { error: error.message });
        await interaction.editReply({
          content: '❌ An error occurred while checking unlinked contributors.',
        });
      }
    },
  },
  {
    // NOTE: adding/removing/renaming a `/qurl` subcommand? Update the
    // expected-set assertion in
    // `e2e/tests/discord-commands.smoke.test.ts` too — the smoke test
    // pins the subcommand NAME set (not option types, requiredness, or
    // descriptions) to catch registration regressions at deploy time.
    // IIFE so the `/qurl map` subcommand can be conditionally added
    // based on config.MAP_COMMAND_ENABLED — a plain fluent chain
    // can't host an `if`. The order of `addSubcommand` calls below
    // matches the chain we'd write inline, with `map` inserted
    // between `send` and `revoke` when the flag is on.
    data: (() => {
      const builder = new SlashCommandBuilder()
        .setName('qurl')
        .setDescription('Share resources securely via qURL')
        .addSubcommand(sub =>
          sub.setName('send')
            .setDescription('Share a file via one-time qURL links')
            .addAttachmentOption(opt =>
              opt.setName('attachment')
                .setDescription('The file to share')
                .setRequired(true)
            )
            .addStringOption(opt =>
              opt.setName('recipients')
                .setDescription('Users to send to — paste @mentions. Leave blank to pick from a menu.')
                .setRequired(false)
                .setMaxLength(RECIPIENTS_SLASH_MAX_LENGTH)
            )
            .addStringOption(opt =>
              opt.setName('expires-in')
                .setDescription('How long the qURL links stay valid (default: 24 hours)')
                .setRequired(false)
                .addChoices(...EXPIRY_CHOICES)
            )
            .addStringOption(opt =>
              opt.setName('self-destruct')
                .setDescription('Optional countdown after first open (default: no timer)')
                .setRequired(false)
                .addChoices(...SELF_DESTRUCT_CHOICES)
            )
            .addStringOption(opt =>
              opt.setName('personal-message')
                .setDescription('Optional note included in each recipient\'s DM')
                .setRequired(false)
                .setMaxLength(PERSONAL_MESSAGE_INPUT_MAX)
            )
        );
      // /qurl detect — watermark attribution (#1101). Gated behind
      // DETECT_COMMAND_ENABLED (default OFF), like /qurl map: the connector
      // /api/detect backend 503/404s until the watermark stack is ACTIVATED (a
      // separate gated step AFTER the connector deploys), so detect stays DARK
      // until an operator flips the flag at activation — no visible-but-failing
      // command in the interim. The smoke test (e2e/tests/discord-commands.smoke.test.ts)
      // reads the same flag and asserts detect present-when-enabled / absent-when-off.
      if (config.DETECT_COMMAND_ENABLED) {
        builder.addSubcommand(sub =>
          sub.setName('detect')
            .setDescription('Find which recipient an image was watermarked for (this server)')
            .addAttachmentOption(opt =>
              opt.setName('image')
                .setDescription('The image to check for a qURL watermark')
                .setRequired(true)
            )
        );
      }
      if (config.MAP_COMMAND_ENABLED) {
        builder.addSubcommand(sub =>
          sub.setName('map')
            .setDescription('Share a Google Maps location via one-time qURL links')
            .addStringOption(opt =>
              opt.setName('location')
                .setDescription('Google Maps URL, or a place / address to search')
                .setRequired(true)
                // Picking a suggestion sends a `qurl_place:<id>` sentinel;
                // free text is resolved server-side via resolveLocation.
                .setAutocomplete(true)
                // 500 chars covers full Google Maps URLs (which can be
                // 200-400 chars after place params + coordinates) plus
                // headroom; not tied to the recipients-parser cap because
                // location is a single URL/address, not a token list.
                .setMaxLength(500)
            )
            .addStringOption(opt =>
              opt.setName('recipients')
                .setDescription('Users to send to — paste @mentions. Leave blank to pick from a menu.')
                .setRequired(false)
                .setMaxLength(RECIPIENTS_SLASH_MAX_LENGTH)
            )
            .addStringOption(opt =>
              opt.setName('location-name')
                .setDescription('Override the label shown to recipients (defaults to URL/address)')
                .setRequired(false)
                .setMaxLength(256)
            )
            .addStringOption(opt =>
              opt.setName('expires-in')
                .setDescription('How long the qURL links stay valid (default: 24 hours)')
                .setRequired(false)
                .addChoices(...EXPIRY_CHOICES)
            )
            .addStringOption(opt =>
              opt.setName('self-destruct')
                .setDescription('Optional countdown after first open (default: no timer)')
                .setRequired(false)
                .addChoices(...SELF_DESTRUCT_CHOICES)
            )
            .addStringOption(opt =>
              opt.setName('personal-message')
                .setDescription('Optional note included in each recipient\'s DM')
                .setRequired(false)
                .setMaxLength(PERSONAL_MESSAGE_INPUT_MAX)
            )
        );
      }
      builder.addSubcommand(sub =>
        sub.setName('revoke')
          .setDescription('Revoke links from a previous send')
      );
      builder.addSubcommand(sub =>
        sub.setName('help')
          .setDescription('Show qURL bot help')
      );
      builder.addSubcommand(sub =>
        sub.setName('setup')
          .setDescription('Configure your qURL API key for this server (admin only)')
      );
      builder.addSubcommand(sub =>
        sub.setName('status')
          .setDescription('Check if qURL is configured (admin only)')
      );
      return builder;
    })(),
    async execute(interaction) {
      const sub = interaction.options.getSubcommand();

      // /qurl setup — admin-only, configure API key for this server.
      //
      // Default flow (when AUTH0_* env vars are configured): replies with
      // a one-shot OAuth-redirect link. Admin clicks → Auth0 sign-in +
      // consent → server-side callback mints a guild-scoped API key on
      // qurl-service (admin-owned, billed to the admin's qURL account)
      // and persists it via the Store abstraction. NO API key paste.
      //
      // Fallback flow (AUTH0_* unset, e.g. early sandbox before Justin
      // registers the Auth0 app): reverts to the legacy modal-paste UX
      // so the bot stays usable until OAuth is wired end-to-end. The
      // fallback can be removed in a follow-up once the OAuth path is
      // live in prod.
      if (sub === 'setup') {
        if (!interaction.guildId) {
          return interaction.reply({ content: 'This command can only be used in a server, not in DMs.', ephemeral: true });
        }
        if (!interaction.memberPermissions?.has(PermissionFlagsBits.ManageGuild)) {
          return interaction.reply({ content: 'Only server administrators can configure qURL.', ephemeral: true });
        }

        // OAuth path — preferred when configured.
        if (config.isQurlOAuthConfigured) {
          // Fail-fast on encryption-at-rest BEFORE minting the OAuth
          // setup link — otherwise the admin clicks through, completes
          // the full Auth0 dance, and only then sees the 503 from
          // /oauth/qurl/start. Same actionable message as the legacy
          // modal-paste branch below; round-9.6 #4 surface symmetry.
          if (!process.env.KEY_ENCRYPTION_KEY) {
            logger.error('Refusing /qurl setup (OAuth path): KEY_ENCRYPTION_KEY is not set');
            return interaction.reply({
              content: '❌ **qURL is not ready to accept setup on this server.**\n\n'
                + 'The bot operator needs to set `KEY_ENCRYPTION_KEY` (encryption-at-rest) before '
                + '/qurl setup can store keys safely. Ask them to check the deployment env.',
              ephemeral: true,
            });
          }
          const state = signQurlOAuthState(interaction.guildId, interaction.user.id);
          const startUrl = `${config.BASE_URL}/oauth/qurl/start?state=${encodeURIComponent(state)}`;
          return interaction.reply({
            content: '🔐 **Connect qURL to this server**\n\n'
              + `**[Click here to authorize qURL](${startUrl})**\n\n`
              + '_Open in this browser; the link expires in 5 minutes._',
            ephemeral: true,
          });
        }

        // Legacy modal-paste fallback. Refuse to accept a guild API key
        // unless encryption-at-rest is configured. Falling through to the
        // crypto module's plaintext fallback would silently store a
        // billing-sensitive secret on disk.
        if (!process.env.KEY_ENCRYPTION_KEY) {
          logger.error('Refusing /qurl setup: KEY_ENCRYPTION_KEY is not set');
          return interaction.reply({
            content: '❌ **qURL is not ready to accept API keys on this server.**\n\n' +
              'The bot operator needs to set `KEY_ENCRYPTION_KEY` (encryption-at-rest) before '
              + '/qurl setup can store keys safely. Ask them to check the deployment env.',
            ephemeral: true,
          });
        }

        // Open the flow row + render the button. The actual modal is
        // shown on button-click by handleSetupButton (see
        // dispatcher-side handlers above), which lets a deploy
        // between command and modal still resume cleanly — the
        // button is a persistent message component, not an
        // in-process Promise.
        //
        // Supersede semantics: supersedeOrCreate's version-gated
        // deleteFlow makes a second /qurl setup a no-op when the
        // prior flow is mid-modal (the surviving peek returns the
        // awaiting_setup_modal row unchanged, and we surface the
        // "modal already open" wording from its registered
        // siblingMessage). When the prior flow is still pre-modal
        // — i.e. the admin walked away from the button — the
        // claim succeeds and they get a fresh button.
        await interaction.deferReply({ ephemeral: true });
        const setupFlowId = flowIdForInteraction(interaction);
        let setupSupersede;
        try {
          setupSupersede = await supersedeOrCreate({
            flow_id: setupFlowId,
            stage: SETUP_STAGE_AWAITING_BUTTON,
            payload: null,
            ttl_seconds: SETUP_BUTTON_TTL_SECONDS,
          });
        } catch (err) {
          logger.warn('handleSetup: supersedeOrCreate threw', {
            flow_id: setupFlowId, error: err && err.message,
          });
          return interaction.editReply({
            content: 'Could not start a setup session — please try again.',
          });
        }
        if (!setupSupersede.created) {
          // Did not claim the slot. Three distinct branches:
          //   (a) surviving.stage === awaiting_setup_modal → admin
          //       has an in-flight modal; tell them to finish or
          //       wait for it to expire.
          //   (b) surviving is a sibling flow's row (e.g. revoke
          //       menu open) → surface its registered sibling
          //       message.
          //   (c) surviving is null OR an unregistered stage →
          //       generic "try again" fallback.
          logger.warn('handleSetup: supersedeOrCreate did not claim slot', {
            flow_id: setupFlowId,
            surviving_stage: setupSupersede.surviving?.stage ?? null,
          });
          const siblingMsg = siblingMessageForStage(setupSupersede.surviving?.stage);
          if (siblingMsg) {
            return interaction.editReply({ content: siblingMsg });
          }
          return interaction.editReply({
            content: 'Could not start a setup session — please try again.',
          });
        }

        const setupButton = new ButtonBuilder()
          .setCustomId(SETUP_BUTTON_CUSTOM_ID)
          .setLabel('Configure qURL')
          .setStyle(ButtonStyle.Primary);
        return interaction.editReply({
          content: '🔐 **Connect qURL to this server**\n\n'
            + 'Click below to open the API-key form. Inputs are NOT recorded in Discord audit logs.',
          components: [new ActionRowBuilder().addComponents(setupButton)],
        });
      }

      // /qurl status — check if configured. Gate behind ManageGuild: the
      // response echoes the last 4 chars of the API key (billing-sensitive)
      // and any guild member could previously run this and snoop them.
      if (sub === 'status') {
        if (!interaction.memberPermissions?.has(PermissionFlagsBits.ManageGuild)) {
          return interaction.reply({
            content: '❌ Only server administrators can view qURL configuration status.',
            ephemeral: true,
          });
        }
        const guildConfig = await db.getGuildConfig(interaction.guildId);
        if (guildConfig) {
          // Show a short sha256 fingerprint instead of any key substring — a
          // 4-char suffix narrows brute-force space and a prefix leaks tenant
          // hints. An 8-char hex fingerprint is enough for an admin to confirm
          // they re-ran setup with the same key, without exposing bytes.
          // getGuildConfig no longer returns the decrypted key (it would
          // leak via any row dump); go through the explicit accessor and
          // let the plaintext fall out of scope immediately after hashing.
          const plaintextKey = await db.getGuildApiKey(interaction.guildId) || '';
          const keyFingerprint = crypto.createHash('sha256')
            .update(plaintextKey)
            .digest('hex')
            .slice(0, 8);

          // #185 admin-offboarding nudge: the qURL key is owned by the
          // admin who ran setup (Auth0 sub claim); usage bills to their
          // qURL account even after they leave. Surface a passive notice
          // so a remaining ManageGuild admin knows to rerun setup to
          // take over billing. Best-effort — a Discord API blip just
          // omits the notice rather than failing the whole status read.
          let originalAdminLeftNotice = '';
          if (guildConfig.configured_by) {
            try {
              await interaction.guild.members.fetch(guildConfig.configured_by);
            } catch (err) {
              // discord.js throws DiscordAPIError code 10007 ("Unknown
              // Member") when the user is no longer in the guild. Any
              // other error (rate limit, transient) → skip the nudge
              // silently rather than mis-flag a present admin.
              if (err?.code === 10007) {
                originalAdminLeftNotice =
                  '\n\n⚠️ The admin who originally ran `/qurl setup` (<@' +
                  guildConfig.configured_by + '>) has left this server. ' +
                  'qURL usage continues to bill to their layerv.ai account. ' +
                  'A current `ManageGuild` admin can run `/qurl setup` again to take over billing.';
              }
            }
          }

          return interaction.reply({
            content: `✅ **qURL is configured**\n` +
              `Key fingerprint: \`${keyFingerprint}\`\n` +
              `Configured by: <@${guildConfig.configured_by}>\n` +
              `Last updated: ${guildConfig.updated_at}` +
              originalAdminLeftNotice,
            ephemeral: true,
          });
        }
        // Branch the not-configured copy on the active setup flow so
        // the instructions match what /qurl setup actually accepts —
        // OAuth-redirect (no api_key arg) vs legacy modal-paste.
        // Pre-OAuth this hardcoded `setup api_key:lv_live_your_key_here`,
        // which never matched the modal flow either; fixed in round-9.6
        // alongside the OAuth-redirect path documentation.
        const notConfiguredCopy = config.isQurlOAuthConfigured
          ? '❌ **qURL is not configured for this server.**\n\n'
            + 'Run `/qurl setup` to connect — you\'ll be redirected to layerv.ai to authorize, '
            + 'and the bot will mint an API key bound to your server. Only server administrators can run setup.'
          : '❌ **qURL is not configured for this server.**\n\n'
            + '1. Sign up at **https://layerv.ai** to get your API key\n'
            + '2. Run `/qurl setup` and paste the key into the modal\n\n'
            + 'Only server administrators can run setup.';
        return interaction.reply({
          content: notConfiguredCopy,
          ephemeral: true,
        });
      }

      // Stale-client rename reply — `/qurl file` was renamed to
      // `/qurl send`. Discord clients cache slash registrations and
      // can submit the old name for up to ~1h after a global rollout
      // (near-instant on guild-scoped registration). Caught here
      // BEFORE API_KEY_GATED_SUBCOMMANDS so unconfigured guilds get
      // the rename hint instead of "qURL is not configured".
      if (sub === 'file') {
        logger.debug('qurl_file_renamed_reply: stale-client /qurl file submission caught by rename gate', {
          user_id: interaction.user?.id,
          guild_id: interaction.guildId,
        });
        return interaction.reply({
          content: '⚠️ `/qurl file` has been renamed to `/qurl send`. Please use `/qurl send` instead — your Discord client may take up to an hour to refresh the command list.',
          ephemeral: true,
        });
      }

      // Gate: require guild API key for send/map/revoke (the set
      // in API_KEY_GATED_SUBCOMMANDS, hoisted to module scope).
      //
      // For /qurl send + /qurl map this read is a fail-fast presence
      // check — the resolved value is intentionally NOT threaded
      // through to the back-half. handleConfirmSendClick re-fetches at
      // Send-click time so a key rotation during the 3-minute confirm-
      // card window is honored. Threading resolvedApiKey through here
      // would break that rotation-safety property: a future optimization
      // pass that "saves the redundant DDB read" would silently
      // re-introduce stale-key dispatches. The handleQurlSlashSend
      // docstring documents the contract explicitly.
      let resolvedApiKey = null;
      if (API_KEY_GATED_SUBCOMMANDS.has(sub)) {
        const guildApiKey = interaction.guildId ? await db.getGuildApiKey(interaction.guildId) : null;
        if (!guildApiKey && !config.QURL_API_KEY) {
          return interaction.reply({
            content: '❌ **qURL is not configured for this server.**\n\n' +
              'A server admin needs to run `/qurl setup` first.\n' +
              'Sign up at **https://layerv.ai** to get your API key.',
            ephemeral: true,
          });
        }
        resolvedApiKey = guildApiKey || config.QURL_API_KEY;
      }

      // /qurl send and /qurl map deliberately don't accept the
      // dispatcher-resolved apiKey — handleConfirmSendClick re-fetches
      // at Send time so a mid-flow rotation still uses the live key.
      // The dispatcher's API_KEY_GATED_SUBCOMMANDS gate above is the
      // fail-fast presence check.
      if (sub === 'send') return handleQurlSend(interaction);
      // /qurl detect resolves its own API key inside the handler (BYOK →
      // global fallback) — deliberately NOT in API_KEY_GATED_SUBCOMMANDS,
      // so it dispatches here without the dispatcher-level key gate.
      if (sub === 'detect') {
        if (!config.DETECT_COMMAND_ENABLED) {
          // Stale client with a cached command def that still lists `detect`
          // after the toggle was off — expected post-deploy traffic, not an
          // error (same cache-TTL story as /qurl map below). Enforcing the flag
          // HERE too means a stale detect can't reveal a recipient even if the
          // backend is live but the flag is off.
          logger.debug('qurl_detect_disabled_reply: stale-client /qurl detect submission caught by toggle gate', {
            user_id: interaction.user?.id,
            guild_id: interaction.guildId,
          });
          return interaction.reply({ content: QURL_DETECT_DISABLED_REPLY, ephemeral: true });
        }
        return handleQurlDetect(interaction);
      }
      if (sub === 'map') {
        if (!config.MAP_COMMAND_ENABLED) {
          // Debug (not warn) — expected post-deploy traffic from
          // clients with cached command defs, not an error. Cache
          // TTL depends on registration mode: up to 1h on global
          // (multi-tenant) registration, near-instant on guild-
          // scoped (single-GUILD_ID). Grep this line to see decay.
          logger.debug('qurl_map_disabled_reply: stale-client /qurl map submission caught by toggle gate', {
            user_id: interaction.user?.id,
            guild_id: interaction.guildId,
          });
          return interaction.reply({ content: QURL_MAP_DISABLED_REPLY, ephemeral: true });
        }
        return handleQurlMap(interaction);
      }
      if (sub === 'revoke') return handleRevoke(interaction, resolvedApiKey);
      if (sub === 'help') {
        // Section order: user-facing flow first (Getting started → How it
        // works), then admin-only setup (now the OAuth-redirect flow per
        // PR #177), then glossary (Terms), then operational caveat
        // The "Setting up" section pivots based on
        // whether OAuth is configured — when it is, we describe the
        // /qurl setup OAuth flow + the "Add to Discord" install-flow
        // entry point. When unset (sandbox before Auth0 secrets land),
        // we keep the legacy "API key paste" wording so the help text
        // matches what /qurl setup actually does at that moment.
        const oauthSetupSection = config.isQurlOAuthConfigured
          ? '**Setting up (for Admins):**\n'
            + '  `/qurl setup` — connect qURL via OAuth (admin only). Click the link, sign in to layerv.ai, consent. No API key paste.\n'
            + '  `/qurl status` — check if qURL is configured (admin only)\n\n'
            + '_Adding the bot to a new server?_ Use the "Add to Discord" link on **https://layerv.ai** — '
            + 'it walks you through server selection, permissions consent, and qURL connection in one click chain.\n\n'
          : '**Setting up (for Admins):**\n'
            + '  `/qurl setup` — configure your API key (admin only)\n'
            + '  `/qurl status` — check if qURL is configured (admin only)\n\n';
        // `cmd` is used in two spots (Terms + Large servers); a
        // single token keeps them in lockstep across copy edits.
        const mapCopy = config.MAP_COMMAND_ENABLED
          ? {
            sectionVerb: 'Share resources',
            bullet: '  `/qurl map` — share a Google Maps location via one-time qURL links\n',
            runLine: '\t1. Run `/qurl send` (attach a file) or `/qurl map` (paste a Google Maps URL or address)\n',
            resource: 'the file or location',
            cmd: '`/qurl send` or `/qurl map`',
          }
          : {
            sectionVerb: 'Share files',
            bullet: '',
            runLine: '\t1. Run `/qurl send` (attach a file)\n',
            resource: 'the file',
            cmd: '`/qurl send`',
          };
        return interaction.reply({
          content: '**qURL Bot — Help**\n\n' +
            `**Getting started — ${mapCopy.sectionVerb} securely via one-time links:**\n` +
            '  `/qurl send` — share a file with users via one-time qURL links\n' +
            mapCopy.bullet +
            '  `/qurl revoke` — revoke links from a previous send\n' +
            '  `/qurl help` — show this message\n\n' +
            '**How it works:**\n' +
            // Leading tab on "1." keeps Discord's markdown parser from
            // treating it as the start of an ordered list (which would
            // renumber it relative to the subsequent lines and visually
            // misalign "1." with "2.", "3.", "4."). The tab indent now
            // matches the two-space indent below, but bypasses the list
            // auto-formatter.
            mapCopy.runLine +
            '  2. Optionally `recipients:@a @b @role` (up to 25 users via @mentions or role expansion) — '
              + 'leave blank to pick from a menu (up to 10 at a time)\n' +
            '  3. Confirm the card, then click **Send**\n' +
            '  4. Recipients get a one-time link by DM that self-destructs on first access (or when the expiry elapses)\n\n' +
            oauthSetupSection +
            `**Terms:** a *protected resource* is ${mapCopy.resource} you're sharing. ` +
            'A *qurl* (or *access link*) is the single-use URL that delivers it. ' +
            `You create a qurl for a protected resource each time you run ${mapCopy.cmd}.\n\n` +
            `**Large servers (~1000+ members):** ${mapCopy.cmd} with role @mentions ` +
            'may skip members the bot has not yet cached locally (the bot fetches members lazily, ' +
            'and very large servers may not be fully populated). ' +
            'If you need to reach a specific person for sure, use an explicit user @mention instead of a role.\n\n' +
            'Learn more at **https://layerv.ai**.',
          ephemeral: true,
        });
      }
    },
  },
];

// Commands that are safe to register outside the OpenNHP community guild.
// Everything else (/link, /whois, /contributions, /stats, /leaderboard,
// /forcelink, /bulklink, /unlinked, /backfill-milestones, /unlink) are
// OpenNHP-community features that depend on single-guild state (the
// cached guild, BASE_URL, GITHUB_* secrets). Registering them outside
// the OpenNHP guild would put them in autocomplete where they'd fail
// opaquely — /link would build a URL with an undefined BASE_URL,
// /forcelink would try to fetch members from a null guild, etc.
//
// The full command set is only registered when the bot is in "OpenNHP
// mode": GUILD_ID points at a real guild AND ENABLE_OPENNHP_FEATURES is
// true. Every other configuration (multi-tenant, OR single-guild-plain
// /qurl install like the test playground or a customer server) gets
// only the allowlist.
//
// Keep the allowlist explicit and near the commands array so adding a
// new customer-safe command requires updating both locations
// intentionally.
const CUSTOMER_SAFE_COMMANDS = new Set(['qurl']);

// Single callsite for the active command set. `registerCommands` (at
// boot) and `handleCommand` (per interaction) both ask this so a future
// gating change — e.g. a third mode, a per-command flag — touches one
// place instead of two. Keeps the two sites from drifting.
function getActiveCommands() {
  return config.isOpenNHPActive
    ? commands
    : commands.filter(cmd => CUSTOMER_SAFE_COMMANDS.has(cmd.data.name));
}

// Proactively clear stale guild-scoped command registrations from any
// guild the bot is in. Discord's guild and global command namespaces do
// not purge each other on a fresh PUT call, so a bot that previously
// ran in OpenNHP mode (GUILD_ID=X, full command set registered to guild
// X) and is now redeployed in multi-tenant or single-guild-plain mode
// will leave /link, /leaderboard, etc. visible in X's slash-command
// autocomplete until Discord's cache ages out. The dispatch-time filter
// in handleCommand prevents those stale commands from doing anything
// harmful, but users still see dead commands in the picker. Issuing
// PUT-with-empty-body on the guild-commands endpoint clears the
// guild-scoped set.
//
// Scoped to non-OpenNHP modes: in OpenNHP mode we intentionally register
// guild-scoped commands to config.GUILD_ID, so purging there would
// race with the upcoming PUT. Only iterates the `guilds` map the caller
// passes in — we can't and shouldn't enumerate guilds we've never joined.
async function purgeStaleGuildCommands({ rest, appId, guilds }) {
  if (config.isOpenNHPActive) return; // guild-scoped register is the goal in OpenNHP mode
  // Parallelize the per-guild fetch+put. Promise.allSettled so one
  // slow/failing guild doesn't block the others; Discord's guild-
  // commands endpoint uses a separate rate bucket per guild.
  await Promise.allSettled(Array.from(guilds, ([guildId, guildName]) => (async () => {
    try {
      const existing = await rest.get(Routes.applicationGuildCommands(appId, guildId));
      const existingCount = Array.isArray(existing) ? existing.length : 0;
      if (existingCount === 0) return;
      await rest.put(Routes.applicationGuildCommands(appId, guildId), { body: [] });
      logger.info(`Purged ${existingCount} stale guild-scoped commands from ${guildName ?? guildId} (${guildId})`);
    } catch (error) {
      // Don't fail boot on a purge error — the dispatch-time filter in
      // handleCommand is the correctness guarantee; purge is UX polish.
      logger.warn(`Could not purge stale commands from guild ${guildId}`, { error: error.message });
    }
  })()));
}

// Register commands with Discord. `config.isOpenNHPActive` is the
// single source of truth for "this deployment exercises the OpenNHP
// community surface" — see config.js for the derivation.
//
// Signature decoupled from discord.js Client so both the legacy
// Client path AND the @discordjs/ws shim path can call this:
//   - Legacy path: build `guilds` as `Map<guildId, guildName>` from
//     client.guilds.cache.
//   - Shim path: empty Map — gateway-tier shim doesn't track guild
//     cache, and the handleCommand dispatch-time filter remains the
//     correctness guarantee for stale registrations.
async function registerCommands({ rest, appId, guilds = new Map() }) {
  await purgeStaleGuildCommands({ rest, appId, guilds });

  const activeCommands = getActiveCommands();
  const commandData = activeCommands.map(cmd => cmd.data.toJSON());

  try {
    if (config.GUILD_ID) {
      // Guild-scoped registration: commands appear instantly in just this
      // guild. Used by the single-guild OpenNHP deployment where fast command
      // iteration matters more than appearing in other guilds.
      logger.info(`Registering ${activeCommands.length} slash commands to guild ${config.GUILD_ID}...`);
      await rest.put(Routes.applicationGuildCommands(appId, config.GUILD_ID), { body: commandData });
    } else {
      // Global registration: commands appear in every guild the bot joins.
      // Discord caches global commands for up to 1 hour, so newly-added
      // commands may take that long to propagate. Used for multi-tenant
      // deployments (customers invite the bot to their own servers).
      logger.info(`Registering ${activeCommands.length} slash commands globally (multi-tenant mode): ${activeCommands.map(c => c.data.name).join(', ')}`);
      await rest.put(Routes.applicationCommands(appId), { body: commandData });
    }
    logger.info('Slash commands registered.');
  } catch (error) {
    logger.error('Failed to register commands', { error: error.message });
  }
}

// Discord's 3 s interaction-acknowledgement deadline surfaces as a
// REST 10062 / "Unknown interaction" error from discord.js when the bot
// tries to reply after the token has expired. Detect both the numeric
// code (preferred — discord.js DiscordAPIError preserves it) and the
// message text (fallback for wrapped-error shapes that drop .code).
// The regex accepts an optional "Class: " or "Class[code]: " prefix
// common to wrapped shapes — e.g. `RESTJSONError: Unknown interaction`
// or discord.js's own `DiscordAPIError[10062]: Unknown interaction` —
// but rejects trailing content like "Unknown interaction type X" to
// avoid misclassification.
const ACK_TIMEOUT_MSG_RE = /^(?:[A-Za-z][A-Za-z0-9]*(?:\[\d+\])?: )?Unknown interaction$/;
function isAckTimeoutError(err) {
  if (!err) return false;
  if (err.code === 10062) return true;
  return typeof err.message === 'string' && ACK_TIMEOUT_MSG_RE.test(err.message);
}

// Below this length, autocomplete suggestions are noise (single-letter
// prefixes match thousands of places) and the per-keystroke Places
// cost isn't justified.
const AUTOCOMPLETE_MIN_QUERY_LENGTH = 2;

// Sampled-warn cadence for autocomplete failures. The catch logs at
// `debug` per-call to avoid keystroke-rate log spam during a Places
// outage; this counter emits one `warn` per AUTOCOMPLETE_FAILURE_LOG_BURST
// failures so SRE has a coarse signal that autocomplete is degraded
// (vs. no traffic) without flooding logs. resolveLocation's send-time
// `warn` carries the load-bearing signal — this is secondary visibility.
const AUTOCOMPLETE_FAILURE_LOG_BURST = 50;
let autocompleteFailureBurst = 0;

// Discord caps each choice's `name` (label) and `value` (handler input)
// at 100 chars. Labels (name + address) often need truncation; values
// (`qurl_place:<placeId>`) almost never do, but we drop pathological
// ones so one bad result doesn't fail the whole response.
const AUTOCOMPLETE_CHOICE_NAME_MAX = 100;
const AUTOCOMPLETE_CHOICE_VALUE_MAX = 100;
const AUTOCOMPLETE_MAX_CHOICES = 25;

// Per Discord's contract, MUST respond within 3 s. Two-layer error
// handling: the inner try catches Places I/O failures (ticks the
// sampled SRE counter; that's its intent — "Places is degraded"); the
// outer try catches anything else (early-return respond([]) throws on
// expired token, etc.) without ticking the counter, since those aren't
// Places-degradation signals.
async function handleAutocomplete(interaction) {
  try {
    // `return await` (not bare `return`) so a rejected `respond([])`
    // promise is caught by the outer try/catch instead of leaking out
    // of the async function unhandled. Each early-return is the
    // contract handler — surface the rejection through the outer
    // recovery path instead of propagating up to the dispatch caller.
    if (interaction.commandName !== 'qurl') {
      return await interaction.respond([]);
    }
    // Reject DM autocomplete — handleQurlMap rejects DMs at submit time
    // (see commands.js:~3502) but Discord could still deliver an
    // autocomplete interaction without a guildId. Without this guard a
    // user who somehow triggered autocomplete in DM would burn the
    // operator's global GOOGLE_MAPS_API_KEY quota for a send that's
    // about to be rejected.
    if (!interaction.guildId) {
      return await interaction.respond([]);
    }
    const subcommand = interaction.options.getSubcommand(false);
    const focused = interaction.options.getFocused(true);
    if (subcommand !== 'map' || focused?.name !== 'location') {
      return await interaction.respond([]);
    }
    // Feature-flag gate. When MAP_COMMAND_ENABLED is off the map
    // subcommand isn't registered, so Discord shouldn't deliver an
    // autocomplete for it — but a stale client racing the registration
    // roll could. Short-circuit before searchPlaces so we don't burn
    // operator GOOGLE_MAPS_API_KEY quota on a submit that will be
    // rejected by the dispatcher anyway.
    if (!config.MAP_COMMAND_ENABLED) {
      return await interaction.respond([]);
    }
    const rawQuery = (focused.value || '').trim();
    if (rawQuery.length < AUTOCOMPLETE_MIN_QUERY_LENGTH) {
      return await interaction.respond([]);
    }
    // A pasted URL is already a stable identifier — parseLocationInput
    // passes it through verbatim and suggestions would just clutter.
    if (/^https?:\/\//i.test(rawQuery)) {
      return await interaction.respond([]);
    }

    let results;
    try {
      results = await searchPlaces(rawQuery);
    } catch (err) {
      // Per-call log at debug so keystroke-rate failures don't spam.
      logger.debug('autocomplete handler failed', {
        command: interaction.commandName,
        error: err && err.message,
      });
      // Sampled SRE signal: emit one warn per BURST failures so a
      // Places outage is visible (vs. silent autocomplete-only degrade).
      if (++autocompleteFailureBurst >= AUTOCOMPLETE_FAILURE_LOG_BURST) {
        logger.warn('autocomplete handler failure burst', {
          count: autocompleteFailureBurst,
          error: err && err.message,
        });
        autocompleteFailureBurst = 0;
      }
      return await interaction.respond([]);
    }

    const choices = [];
    for (const p of results) {
      if (choices.length >= AUTOCOMPLETE_MAX_CHOICES) break;
      // Skip dud entries early instead of relying on submit-time decode
      // to reject them — saves the user a "place no longer available"
      // error on a malformed Google response.
      if (!PLACE_ID_SHAPE_RE.test(p.placeId)) {
        // Debug, not warn: would fire per-keystroke during a Google
        // place_id format drift, drowning logs. Operator can grep for
        // this when investigating "why is my dropdown empty?" — that's
        // the upstream-shape-drift signal.
        logger.debug('autocomplete: dropped prediction (place_id failed shape check)', {
          place_id: p.placeId,
        });
        continue;
      }
      // Places marks `main_text` and `description` as optional; if both
      // are missing, searchPlaces returns `name: undefined` and the
      // label would render as the literal string "undefined". Discord
      // also rejects empty/whitespace names, so just skip.
      if (!p.name) continue;
      const value = encodePlaceIdSentinel(p.placeId);
      if (value.length > AUTOCOMPLETE_CHOICE_VALUE_MAX) continue;
      const label = p.address ? `${p.name} — ${p.address}` : p.name;
      // Discord validates name length in UTF-16 code units, not code
      // points, so we cap by `.length`. Back off by 1 if the boundary
      // would leave a lone high surrogate so we don't ship a
      // half-emoji (which Discord renders as tofu). Deliberately NOT
      // `safeCodepointSlice` — that helper counts codepoints, which
      // can ship a string whose `.length` > 100 for emoji-heavy
      // labels (each surrogate pair is 2 UTF-16 units but 1 codepoint).
      // Always-check (not just on truncation): a label that's exactly
      // 100 UTF-16 units AND ends with a lone high surrogate would
      // otherwise slip through the fast path.
      let end = Math.min(label.length, AUTOCOMPLETE_CHOICE_NAME_MAX);
      if (end > 0) {
        const lastUnit = label.charCodeAt(end - 1);
        if (lastUnit >= 0xD800 && lastUnit <= 0xDBFF) end -= 1;
      }
      const name = end === label.length ? label : label.slice(0, end);
      choices.push({ name, value });
    }
    return await interaction.respond(choices);
  } catch (err) {
    // Non-Places-I/O failure (respond() on expired token, getSubcommand
    // throw on malformed interaction, etc.). Doesn't advance the burst
    // counter — those aren't Places-degradation signals.
    logger.debug('autocomplete handler unhandled', {
      command: interaction.commandName,
      error: err && err.message,
    });
    try { await interaction.respond([]); } catch { /* ignore */ }
    return undefined;
  }
}

// Handle command interactions
async function handleCommand(interaction) {
  // Autocomplete fires per-keystroke and must respond within 3s. Route
  // it off the chat-input path so the autocomplete handler doesn't drag
  // in the metrics + mode-flip-defense overhead that handleQurlSlashSend
  // and friends need.
  if (interaction.isAutocomplete()) return handleAutocomplete(interaction);

  if (!interaction.isChatInputCommand()) return;

  // handler_duration_ms is wall-clock from entry to emit, NOT user-
  // perceived ACK latency — for commands that defer + run a background
  // task, this captures the whole operation. Edge-to-ACK is Phase 2.
  // hrtime.bigint() instead of Date.now() so an NTP step backward
  // can't produce a negative duration.
  const handlerStart = process.hrtime.bigint();
  const commandName = interaction.commandName;
  const emitInteractionMetric = (success, failureType) => {
    // Number(ns) / 1_000_000 (NOT BigInt division then Number) so a
    // sub-millisecond handler reports a fractional ms instead of
    // truncating to 0.
    const handler_duration_ms = Number(process.hrtime.bigint() - handlerStart) / 1_000_000;
    logger.audit(AUDIT_EVENTS.INTERACTION_HANDLED, {
      command_name: commandName,
      success,
      failure_type: failureType,
      handler_duration_ms,
    });
  };

  // Defense-in-depth for mode-flip: if an operator switches from OpenNHP
  // mode to customer-safe mode (flip GUILD_ID unset OR flip
  // ENABLE_OPENNHP_FEATURES to false), the prior guild-scoped /link,
  // /whois, etc. registrations remain in the old guild — Discord's two
  // namespaces (guild and global) don't purge each other on a new .set()
  // call. Those stale handlers all assume cached guild state (BASE_URL,
  // contributor roles) that customer-safe mode doesn't populate and
  // would crash on. Filter the handler lookup to the active set so a
  // stale registration from a previous deploy can't dispatch to a broken
  // path.
  const activeCommands = getActiveCommands();
  const command = activeCommands.find(cmd => cmd.data.name === interaction.commandName);
  if (!command) {
    // The interaction is for a command we know exists globally (Discord
    // only dispatches registered commands to us) but is not in the
    // currently-active set — so it's a stale guild-scoped registration
    // from a previous deploy in a different mode. Acknowledge the
    // interaction so the user sees a clear "no longer available" reply
    // instead of Discord's 3-second "This interaction failed" timeout.
    // Defensive: wrap in try/catch since the interaction may already
    // have been responded to by a race elsewhere.
    try {
      await interaction.reply({
        content: 'This command is no longer available in this server.',
        ephemeral: true,
      });
      emitInteractionMetric(false, 'unknown_command');
    } catch (err) {
      logger.warn('Failed to reply to stale command interaction', {
        command: interaction.commandName, error: err.message,
      });
      // Reply throw within Discord's 3 s window surfaces as the user-
      // visible "did not respond" dialog. Tag it distinctly so the
      // ack_timeout alarm catches it without being washed out by other
      // failures.
      emitInteractionMetric(false, isAckTimeoutError(err) ? 'ack_timeout' : 'reply_failed');
    }
    return;
  }

  try {
    await command.execute(interaction);
    emitInteractionMetric(true, null);
  } catch (error) {
    logger.error(`Error executing ${interaction.commandName}`, { error: error.message });
    // Any throw after setCooldown (e.g. deferReply failure, modal timeout path
    // that bubbles) would leave the user in a 30s block for an action that
    // never completed. clearCooldown is idempotent and a no-op for commands
    // that don't use it, so call unconditionally.
    clearCooldown(interaction.user.id);
    const reply = {
      content: 'There was an error executing this command.',
      ephemeral: true,
    };

    // failureType reflects the FIRST failure observed. If the follow-up
    // reply also fails for a non-ack reason, keep the original
    // handler_error — execute() is the more meaningful signal. Only
    // ack_timeout on the reply may override (user-visible "did not
    // respond"). Asymmetric vs. the stale-registration path above
    // (which tags reply_failed) because there's no prior execute() there.
    let failureType = isAckTimeoutError(error) ? 'ack_timeout' : 'handler_error';
    try {
      if (interaction.replied || interaction.deferred) {
        await interaction.followUp(reply);
      } else {
        await interaction.reply(reply);
      }
    } catch (replyError) {
      logger.error('Failed to send error response', { error: replyError.message });
      if (isAckTimeoutError(replyError)) failureType = 'ack_timeout';
    }
    emitInteractionMetric(false, failureType);
  }
}

// Wire flow handlers into the dispatcher at module-load time.
// flow-dispatch's `registerFlow` is the single source of truth for
// the customId → handler routing table; doing this here (rather than
// in index.js) co-locates the registration with the handler
// implementation so future readers don't have to grep two modules to
// understand the routing.
// Each registerFlow co-locates two things at the same site: the
// customId → handler binding (for dispatch) and the stage →
// siblingMessage binding (for cross-flow supersede disambiguation).
// A future flow that adds a new awaiting_* stage SHOULD register
// its siblingMessage here so peer flows' supersede peeks can
// surface actionable wording instead of falling through to the
// generic "try again." Stages without a registered message (e.g.
// short-lived ones nobody else would race against) are fine to
// omit — peers will fall through to the generic copy.
registerFlow(REVOKE_SELECT_CUSTOM_ID, {
  expectedStage: REVOKE_STAGE_AWAITING_SELECT,
  handler: handleRevokeSelect,
  siblingMessage: 'You have a `/qurl revoke` menu open in this channel — finish or cancel it first.',
});
registerFlow(SETUP_BUTTON_CUSTOM_ID, {
  expectedStage: SETUP_STAGE_AWAITING_BUTTON,
  handler: handleSetupButton,
  // Cross-flow collision wording: a /qurl revoke supersede whose
  // peek finds a row at awaiting_setup_button surfaces this — its
  // surviving.stage !== awaiting_revoke_select so it doesn't claim,
  // but a generic "try again" wouldn't tell the admin what to do.
  // (A same-flow-type /qurl setup rerun never hits this path: its
  // own supersedeOrCreate matches the button stage and version-
  // gated-deletes the prior row in one round-trip.)
  siblingMessage: 'You have a `/qurl setup` button waiting in this channel — click it or wait for it to expire.',
});
registerFlow(SETUP_MODAL_CUSTOM_ID, {
  expectedStage: SETUP_STAGE_AWAITING_MODAL,
  handler: handleSetupModal,
  siblingMessage: 'You already have a `/qurl setup` modal open — finish that one, or wait for it to expire.',
});

// /qurl send + /qurl map confirm-card components. All three customIds
// share the same expectedStage — they're three component types
// (button + user-select + button) attached to the same confirm-card
// message, all routed by stage. siblingMessage is registered on the
// FIRST customId only; flow-dispatch rejects mismatched re-registrations
// of the same stage so we don't repeat it.
registerFlow(CONFIRM_USER_SELECT_CUSTOM_ID, {
  expectedStage: SEND_STAGE_AWAITING_CONFIRM,
  handler: handleConfirmUserSelect,
  siblingMessage: 'You have a `/qurl send` or `/qurl map` confirm card open in this channel — finish or cancel it first.',
});
// siblingMessage intentionally omitted on the SEND + CANCEL custom-
// id registrations below — flow-dispatch's `siblingMessages` map is
// keyed by stage (not by customId), so the message registered on
// USER_SELECT above is reachable from any of the three customIds
// at SEND_STAGE_AWAITING_CONFIRM. The "siblingMessage keyed by stage"
// test in qurl-send-map.test.js pins this contract.
registerFlow(CONFIRM_SEND_CUSTOM_ID, {
  expectedStage: SEND_STAGE_AWAITING_CONFIRM,
  handler: handleConfirmSendClick,
});
registerFlow(CONFIRM_CANCEL_CUSTOM_ID, {
  expectedStage: SEND_STAGE_AWAITING_CONFIRM,
  handler: handleConfirmCancelClick,
});
// Confirm-card menus + note button/modal. All share the same
// expectedStage as the original three customIds above; siblingMessage
// is keyed by stage (registered on USER_SELECT only), so no
// re-registration here. Each handler reads `row.payload` from the
// dispatcher's loadFlow and uses `expectedVersion: row.version` on
// transitionFlow for the picker-vs-menu race.
registerFlow(CONFIRM_EXPIRY_SELECT_CUSTOM_ID, {
  expectedStage: SEND_STAGE_AWAITING_CONFIRM,
  handler: handleConfirmExpirySelect,
});
registerFlow(CONFIRM_SELF_DESTRUCT_SELECT_CUSTOM_ID, {
  expectedStage: SEND_STAGE_AWAITING_CONFIRM,
  handler: handleConfirmSelfDestructSelect,
});
registerFlow(CONFIRM_NOTE_BUTTON_CUSTOM_ID, {
  expectedStage: SEND_STAGE_AWAITING_CONFIRM,
  handler: handleConfirmNoteButton,
});
registerFlow(CONFIRM_NOTE_MODAL_CUSTOM_ID, {
  expectedStage: SEND_STAGE_AWAITING_CONFIRM,
  handler: handleConfirmNoteModal,
});
// Voice-everyone button — only present on the confirm card when the
// slash command was invoked from a voice / stage-voice channel.
// Same stage + siblingMessage-keyed-by-stage contract as the other
// CONFIRM_* registrations above.
registerFlow(CONFIRM_VOICE_EVERYONE_BUTTON_CUSTOM_ID, {
  expectedStage: SEND_STAGE_AWAITING_CONFIRM,
  handler: handleConfirmVoiceEveryone,
});
// "Pick people instead" — voice/everyone-mode → picker-mode toggle.
// Same stage contract as the other CONFIRM_* registrations.
registerFlow(CONFIRM_PICK_MANUAL_BUTTON_CUSTOM_ID, {
  expectedStage: SEND_STAGE_AWAITING_CONFIRM,
  handler: handleConfirmPickManual,
});
// @everyone button — only rendered when sender has MENTION_EVERYONE
// and recipientMode is PICKER. Same stage + siblingMessage-keyed-by-
// stage contract as the other CONFIRM_* registrations.
registerFlow(CONFIRM_EVERYONE_BUTTON_CUSTOM_ID, {
  expectedStage: SEND_STAGE_AWAITING_CONFIRM,
  handler: handleConfirmEveryone,
});

module.exports = {
  commands,
  registerCommands,
  handleCommand,
  // Exported for the dispatcher's unit tests. Production code reaches
  // these via flow-dispatch's registry, not these exports.
  handleRevokeSelect,
  handleSetupButton,
  handleSetupModal,
  handleConfirmUserSelect,
  handleConfirmSendClick,
  handleConfirmCancelClick,
  handleConfirmExpirySelect,
  handleConfirmSelfDestructSelect,
  handleConfirmNoteButton,
  handleConfirmNoteModal,
  handleConfirmVoiceEveryone,
  handleConfirmPickManual,
  handleConfirmEveryone,
  verifyStateBinding,
  // _test is only exported in non-production so live state (sendCooldowns)
  // and internal handlers can't leak into prod consumers. Tests run with
  // NODE_ENV=test (jest's default); production deploys set NODE_ENV=production.
  ...(process.env.NODE_ENV !== 'production' && {
    _test: {
      isGoogleMapsURL,
      sanitizeFilename,
      sanitizeMessage,
      isAllowedFileType,
      isOnCooldown,
      setCooldown,
      clearCooldown,
      revokingSendLocks,
      batchSettled,
      expiryToISO,
      sendCooldowns,
      // Renderer exposed so tests can pin the bottom-row button
      // composition directly (e.g., the DM-context @everyone-gate
      // assertion) without driving through handleQurlSend's entry path.
      renderConfirmCardRows,
      // Memo for the @everyone non-bot count is module-scoped. Tests
      // that reuse a guild reference across cases could otherwise get
      // surprising memo hits; exposing the WeakMap lets a beforeEach
      // hook clear specific entries. Production callers never need
      // this — the memo busts on cache.size/memberCount changes.
      _everyoneCountMemo: everyoneCountMemo,
      computeEveryoneDisplayCount,
      handleAddRecipients,
      buildDeliveryPayload,
      buildDeliveryEmbed,
      buildStepThroughButton,
      buildTrustButton,
      packBulkDeliveryComponents,
      buildRevokedDMPayload,
      persistDispatchResult,
      resolveSenderAlias,
      resolveGuildProvenance,
      safeUrlHost,
      // Back-half functions exposed for direct unit testing. Without these
      // hooks, coverage of the polling/revoke/add-recipients code paths
      // can only be reached via full /qurl send + /qurl map integration
      // tests, which require mocking the entire state-machine front-half
      // before the back-half even runs. Direct exposure means each
      // function gets a focused spec without that setup overhead.
      monitorLinkStatus,
      revokeAllLinks,
      renderRevokeMsg,
      renderSendConfirm,
      // Pure view-counter render, re-exported (defined in
      // ./view-counter-render) so the wording/floor contract is pinned
      // directly (degraded→baseMsg, normal→counter line, pending floors
      // at 0) rather than only via the monitor closure. The webhook
      // fast-path imports the same module function; the unit test is the
      // byte-identity anchor both render sites lean on.
      renderViewCounter,
      REVOKE_TRUNC_LIMIT,
      mintLinksInBatches,
      _resetConnectorMintLinksPerRequest: () => { connectorMintLinksPerRequest = TOKENS_PER_RESOURCE; },
      activeMonitors,
      // The top-level back-half driver. Exported here so PR 7b's
      // tests (and the follow-up direct unit spec in #278) can pin
      // its contract against a constructed param object — without
      // re-driving the full /qurl send + /qurl map confirm-card flow.
      executeSendPipeline,
      // Test-only file-concurrency hooks. The slot counter is module-
      // private (live state) and exposing a setter lets the cap branch
      // be tested without a parallel-send harness.
      getActiveFileSends: () => activeFileSends,
      setActiveFileSends: (n) => { activeFileSends = n; },
      // The ACK_TIMEOUT_MSG_RE fallback shape drives the failure_type
      // alarm — table-driven tests pin every shape so a silent regex
      // breakage can't slip through.
      isAckTimeoutError,
      // largeSendThreshold formula has non-trivial math + a
      // degenerate-cap guard; exposed for direct unit testing
      // rather than asserting via log-spy.
      largeSendThreshold,
      LARGE_SEND_RECIPIENT_FLOOR,
      // Setup-flow constants — exposed so tests assert against
      // production values, not stale duplicates that would silently
      // pass when the constants are tuned. The regex export lets
      // future tests assert format shape directly rather than via
      // VALID_KEY round-tripping.
      SETUP_BUTTON_TTL_SECONDS,
      SETUP_MODAL_TTL_SECONDS,
      SETUP_API_KEY_REGEX,
      SETUP_API_KEY_MIN_LENGTH,
      SETUP_API_KEY_MAX_LENGTH,
      SETUP_SUCCESS_MSG,
      // /qurl send + /qurl map handlers + flow constants. Tests pin
      // against production values via these exports rather than
      // re-stating the strings.
      handleQurlSend,
      handleQurlMap,
      // /qurl detect handler + its per-(guild,user) cooldown surface
      // (#1101). The cooldown Map + predicates are exposed so tests pin
      // the abuse-guard contract (composite key, separate bucket from
      // sendCooldowns) directly, like the send cooldowns above.
      handleQurlDetect,
      detectCooldowns,
      isOnDetectCooldown,
      setDetectCooldown,
      clearDetectCooldown,
      // The 60s time-sweep, exposed so the windows-differ regression (detect
      // entry must NOT be swept at the send window when DETECT > SEND) is
      // testable directly. DETECT_NO_MATCH_MSG is exposed so tests pin that
      // the two no-match branches share byte-identical copy.
      sweepCooldowns,
      DETECT_NO_MATCH_MSG,
      resolveRecipientUsers,
      partitionRecipients,
      resolveMentionableSelection,
      resolveRoleNames,
      selfDestructOptionToSeconds,
      renderRecipientWarnings,
      renderConfirmCardContent,
      parseLocationInput,
      resolveLocation,
      RESOLVE_REASON,
      // Disabled-state reply for stale /qurl map submissions. Exported
      // so flag-off tests pin against the production string.
      QURL_MAP_DISABLED_REPLY,
      QURL_DETECT_DISABLED_REPLY,
      handleAutocomplete,
      // Test-only reset: the autocomplete-failure burst counter is
      // module-level state that accumulates across tests within a
      // file unless explicitly cleared.
      _resetAutocompleteFailureBurst: () => { autocompleteFailureBurst = 0; },
      AUTOCOMPLETE_FAILURE_LOG_BURST,
      safeDecodeURIComponent,
      SEND_STAGE_AWAITING_CONFIRM,
      // Every confirm-card customId is exported so the contract test
      // in qurl-send-map.test.js can pin every wire value — a typo
      // in any of these silently breaks routing for in-flight confirm
      // cards, so they need to be test-asserted.
      CONFIRM_USER_SELECT_CUSTOM_ID,
      CONFIRM_SEND_CUSTOM_ID,
      CONFIRM_CANCEL_CUSTOM_ID,
      CONFIRM_EXPIRY_SELECT_CUSTOM_ID,
      CONFIRM_SELF_DESTRUCT_SELECT_CUSTOM_ID,
      CONFIRM_NOTE_BUTTON_CUSTOM_ID,
      CONFIRM_NOTE_MODAL_CUSTOM_ID,
      CONFIRM_VOICE_EVERYONE_BUTTON_CUSTOM_ID,
      CONFIRM_PICK_MANUAL_BUTTON_CUSTOM_ID,
      RECIPIENT_MODE_PICKER,
      RECIPIENT_MODE_VOICE,
      RECIPIENT_MODE_EVERYONE,
      normalizeRecipientMode,
      SEND_FLOW_TTL_SECONDS,
      SELF_DESTRUCT_NO_TIMER_CHOICE,
      // classifyMintFailure: exposed so a unit test pins the
      // reason-category contract (qurl-integrations#276). The
      // cardinality-discipline comment on the helper says "don't
      // add per-API-code branches" — without a test, that's just
      // a docstring waiting to drift.
      classifyMintFailure,
    },
  }),
};
