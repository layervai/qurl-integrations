const { GUILD_SETUP_REQUIRED_REPLY } = require('./utils/guild-installation');
const {
  MessageFlags,
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
const crypto = require('crypto');
const config = require('./config');
const db = require('./store');
const { readFlowRecipients, withFlowRecipients } = require('./flow-recipient-payload');
const { deferUpdateOrStop } = require('./utils/interaction-ack');
const logger = require('./logger');
const {
  COLORS,
  TIMEOUTS,
  RESOURCE_TYPES,
  DM_STATUS,
  MAX_FILE_SIZE,
  MINT_BATCH_SIZE,
  REVOKE_BATCH_MAX_IDS,
  MAX_OVERFLOW_REVOKE_IDS,
  MAX_CONCURRENT_MONITORS,
  DISCORD_MEMBERS_PAGE_SIZE,
  PREWARM_MAX_PAGES,
  AUDIT_EVENTS,
  TRUST,
  ddbSendConfigGuardActionCount,
  ddbSendConfigGuardFitsTransaction,
} = require('./constants');
const {
  expiryToISO,
  expiryToMs,
  formatSelfDestructLabel,
  formatSelfDestructTimingNote,
  formatSelfDestructSegment,
  isPositiveFinite,
  selfDestructSelectValueToSeconds,
  isLegitimateSelfDestructSelectValue,
  SELF_DESTRUCT_PRESETS,
  SELF_DESTRUCT_NO_TIMER_VALUE,
  settlesWithin,
} = require('./utils/time');
const {
  resourceIdLogRef,
} = require('./utils/resource-id');
const {
  downloadAndUpload,
  mintLinks,
  uploadJsonToConnector,
  isAllowedSourceUrl,
  revokeMintedLinks,
} = require('./connector');
const {
  qurlIdForCleanup,
  hasPersistableQurlIdShape,
} = require('./utils/qurl-id');
const {
  deleteFlow,
  loadFlow,
  transitionFlow,
  supersedeOrCreate,
} = require('./flow-state');
const {
  warnFlowCorruptionOnce,
} = require('./flow-warning');
const {
  flowIdForInteraction,
  siblingMessageForStage,
} = require('./flow-dispatch');
const {
  findPlaceFromText,
  getPlaceDetails,
  buildPlaceUrl,
  decodePlaceIdSentinel,
} = require('./places');
const {
  sendDM,
} = require('./discord');
const {
  editDM,
  sendChannelMessage,
} = require('./discord-rest');
const {
  renderViewCounter,
} = require('./view-counter-render');
const {
  DISPLAY_NAME_FALLBACK,
  sanitizeFilename,
  escapeDiscordMarkdown,
  sanitizeDisplayName,
  sanitizeDisplayNamePlain,
  sanitizeContentLabel,
  stripBidiAndControls,
} = require('./utils/sanitize');
const {
  parseRecipientMentions,
  isVoiceChannelType,
  isBotMember,
} = require('./recipient-parser');
const {
  REVOKE_TRUNC_LIMIT,
  REVOKE_CONTENT_SAFE_MAX,
  renderRevokeContent,
} = require('./revoke-render');


// Absolute floor above which a single send earns a `WARN`-level
// audit log at executeSendPipeline entry. 1000 chosen as the cliff
// where DM fan-out at Discord's ~5/sec per-bot limit starts taking
// minutes (1000 / 5 = ~3 min) and mint requests grow with the batch count.
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
  // The connector answered 200 but its own upstream qURL create failed; name
  // that hop instead of bucketing on the upstream status. Gated on the local
  // redaction marker only assertUploadResult sets, never on apiCode alone:
  // other paths parse apiCode out of response bodies, so a 5xx body claiming
  // `qurl_creation_failed` must still bucket as upstream_5xx.
  if (error.apiDetailRedacted === true && error.apiCode === 'qurl_creation_failed') {
    return 'upstream_create_failed';
  }
  const status = error.status ?? 0;
  if (status >= 500 && status < 600) return 'upstream_5xx';
  if (status >= 400 && status < 500) return 'upstream_4xx';
  return 'unknown';
}

// Only an apiDetail that connector.js bounded and redacted itself
// (assertUploadResult → redactUploadApiDetail, which sets apiDetailRedacted)
// is log-safe. Gate on that local marker, never on apiCode: other paths parse
// apiCode out of response bodies, so a body claiming `qurl_creation_failed`
// must not smuggle raw apiDetail text onto the ERROR lines that call this.
function logSafeApiDetail(error) {
  return error?.apiDetailRedacted === true && typeof error.apiDetail === 'string'
    ? { apiDetail: error.apiDetail }
    : {};
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

// --- qURL send helpers ---

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

// Prewarm before recipient expansion. A completed HTTP-only walk is reusable for
// 60 seconds while its cache object and size remain unchanged. Failed/partial
// walks never establish freshness; exact gateway member counts keep their gate.
// shortcut: HTTP-only membership can lag by 60 seconds; use gateway churn events if tighter freshness is required.
//
// In-flight de-duplication via the `prewarmInFlight` map below: two
// concurrent `/qurl send @everyone` invocations in the same guild from
// a cold cache share the same underlying pagination promise. Without
// coalescing, abuse via concurrent invocations could burn REST budget
// linearly. The map is keyed by `guild.id` so cross-guild calls still
// proceed in parallel.
const prewarmInFlight = new Map();
const completedPrewarms = new WeakMap();
const PREWARM_FRESH_MS = 60_000;
// `logCtx` is spread first in every log payload below so explicit
// fields after it (`guild_id`, `cache_size`, etc.) override any
// matching key in the caller's `logCtx`. Don't put `guild_id` in
// `logCtx` at call sites expecting it to win — it won't.
async function prewarmGuildMembersCache(guild, logCtx, onProgress) {
  if (!guild || !guild.members || typeof guild.members.list !== 'function') return;
  const cacheSize = guild.members.cache?.size ?? 0;
  // Only short-circuit on EXACT `memberCount` (gateway tier). The
  // `approximateMemberCount` value can underreport by hundreds — if the
  // cache happened to be partially warmed by per-user `members.fetch(id)`
  // calls from `resolveRecipientUsers`, an approximate-based gate could
  // fire prematurely and underresolve @everyone, re-introducing this
  // PR's original bug in a quieter form. http-only tier (no
  // `memberCount`) can only reuse a recently completed walk below.
  if (guild.memberCount != null && cacheSize >= guild.memberCount) {
    // Observability hook for the hot-cache short-circuit firing. Without
    // this, dashboards can't correlate "how often is the cache warm" with
    // memory growth on large-guild workers.
    logger.debug('members pre-warm short-circuited (hot cache)', {
      ...logCtx, guild_id: guild.id, cache_size: cacheSize, member_count: guild.memberCount,
    });
    return;
  }
  const listener = onProgress ? { notify: onProgress, pending: null } : null;
  const existing = prewarmInFlight.get(guild.id);
  if (existing) {
    if (listener) existing.progress.add(listener);
    try { return await existing.promise; }
    finally {
      existing.progress.delete(listener);
      await listener?.pending;
    }
  }
  const completed = completedPrewarms.get(guild);
  const age = completed ? Date.now() - completed.at : -1;
  if (guild.memberCount == null && completed?.cache === guild.members.cache
      && completed.size === cacheSize && age >= 0 && age < PREWARM_FRESH_MS) {
    logger.debug('members pre-warm short-circuited (recent full walk)', {
      ...logCtx, guild_id: guild.id, cache_size: cacheSize, age_ms: age,
    });
    return;
  }
  completedPrewarms.delete(guild);
  const progress = new Set(listener ? [listener] : []);
  // Defer the first SDK call until the in-flight entry exists, even on a synchronous throw.
  const inFlight = Promise.resolve().then(async () => {
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
        if (batch.size === DISCORD_MEMBERS_PAGE_SIZE && (page + 1) % 10 === 0 && progress.size > 0) {
          const pages = page + 1;
          for (const waiter of progress) {
            if (waiter.pending) continue; // At most one edit per waiter; never queue stale progress.
            waiter.pending = Promise.resolve().then(() => waiter.notify(pages)).catch(() => {
              logger.warn('members pre-warm progress update failed', { caller: logCtx?.caller, pages });
            }).finally(() => { waiter.pending = null; });
          }
        }
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
        if (guild.members.cache && typeof guild.members.cache.size === 'number') {
          completedPrewarms.set(guild, {
            at: Date.now(), cache: guild.members.cache, size: guild.members.cache.size,
          });
        }
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
  });
  prewarmInFlight.set(guild.id, { promise: inFlight, progress });
  try { return await inFlight; }
  finally {
    progress.delete(listener);
    await listener?.pending;
  }
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
// pressure under burst. Each download/upload can hold a full attachment;
// cap here rather than trust users to
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
// Same-process per-sender/send Revoke lock. Collector-local `revokeInFlight`
// handles duplicate clicks inside one management collector; this Set lets
// another collector or select handler in the same process see a Revoke mutating the
// send. Cross-process safety relies on revoked_at plus the guarded
// recordQURLSendBatch transaction.
const revokingSendLocks = new Set();
const ADD_RECIPIENTS_IN_PROGRESS_MSG = 'Already processing an "Add Recipients" action.';
const ALREADY_REVOKING_SEND_MSG = 'Already revoking links for this send.';

const sendCooldowns = new Map();

// Hard ceiling shared by send and detect; expired entries use the time sweep.
const COOLDOWNS_MAX = 20000;

// Trim only excess entries. A normal insertion evicts at most one oldest key;
// callers delete+re-set on touch so active entries move to the tail.
function evictOldest(map, max) {
  while (map.size > max) map.delete(map.keys().next().value);
}

function isOnCooldown(userId) {
  const last = sendCooldowns.get(userId);
  if (!last) return false;
  return Date.now() - last < config.QURL_SEND_COOLDOWN_MS;
}

function setCooldown(userId) {
  // LRU-ish behavior: delete first so set() re-inserts at the end of the
  // Map's insertion order. At capacity, only the oldest excess key rolls out.
  sendCooldowns.delete(userId);
  sendCooldowns.set(userId, Date.now());
  evictOldest(sendCooldowns, COOLDOWNS_MAX);
}

function clearCooldown(userId) {
  sendCooldowns.delete(userId);
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
// embed copy is intentionally evocative ("opened a door", "Closing time")
// rather than literal ("shared a file with you") — the brand goal is to
// convey the qURL hidden-layer model, not just announce a file transfer.
// Links within Discord's 512-character component limit render as the
// original `🚪 Step Through` and trust buttons. Longer qv2 links render
// both actions as equal-weight Markdown links inside the embed so
// Discord accepts the DM without changing the qURL itself.
//
// `senderAlias` is the sender's friendly display name (Discord nickname
// > globalName > username) sourced from resolveSenderAlias.
// `personalMessage` is optional caller-provided context; if present, it
// renders as an italicized blockquote between the sender line and the
// expiry line.
//
// Returns the full Discord message options object (`embeds` + `components`).
// Callers pass the returned payload directly to `sendDM`.
//
// Rendered output (blank rows = Discord's natural section spacing,
// NOT literal `\n` separators — descLines.join('\n') is single-newline):
//
//     ┌─────────────────────────────────────────────────────────────┐
//     │  qURL · APP · Today at 2:47 PM                              │  (Discord-rendered header)
//     │                                                             │
//     │  Vik opened a door for you.                                 │  (description, line 1)
//     │  > "Quarterly numbers — for your eyes only."                │  (description, line 2 — optional italic blockquote)
//     │  🕐 Closing time: in 1 day                                  │  (description, line 3 — Discord <t:N:R>, auto-updates
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
// for marginal aesthetic gain.
// Requested lifetimes are shared by slash choices and the confirm form.
// Recipient closing times come from the mint response, not these labels.
const EXPIRY_LABELS = {
  '30m': '30 minutes',
  '1h': '1 hour',
  '6h': '6 hours',
  '24h': '24 hours',
  '3d': '3 days',
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

// Earlier saved sends/cards can request 7d; the mint's actual expiry still wins.
function isValidSavedExpiry(v) {
  return isValidExpiry(v) || v === '7d';
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

// Discord rejects an entire message when any Link button URL exceeds
// 512 characters. Current qv2 links can exceed that component-specific
// limit while remaining well within the embed-description limit.
// TODO(upstream-contract): Discord API Link Button URL maximum.
const DISCORD_LINK_BUTTON_URL_MAX = 512;

function requiresMarkdownDeliveryActions(qurlLink) {
  return qurlLink.length > DISCORD_LINK_BUTTON_URL_MAX;
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

// Composes the embed only (no button row). The optional Markdown actions
// are used when a qv2 link exceeds Discord's Link-button URL limit. The
// single-path call sites use buildDeliveryPayload below.
function buildDeliveryEmbed({
  senderAlias, guildName, guildIconUrl, qurlLink, expiresAt, personalMessage,
  renderActionsAsMarkdown = false,
}) {
  const destinationDomain = qurlLink == null ? TRUST.DESTINATION_DOMAIN : new URL(qurlLink).hostname;
  if (!destinationDomain) throw new TypeError('Delivery link must have a hostname');
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
  // A noun label stays grammatical when Discord's relative timestamp flips
  // from future to past, even if no expiry webhook edit reaches this DM.
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
  descLines.push(`🕐 Closing time: <t:${expiresAt}:R>`);
  if (renderActionsAsMarkdown) {
    descLines.push(`[🚪 Step Through](${qurlLink}) · [🛡️ What is qURL?](${TRUST.LANDING_URL})`);
  }

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
    .setFooter({ text: `opens ${destinationDomain}` });
}

// Convenience wrapper used by executeSendPipeline. Short links preserve
// the original [Step Through, What is qURL?] ActionRow; long qv2 links
// place both actions in the embed and return no components.
function buildDeliveryPayload({ senderAlias, guildName, guildIconUrl, qurlLink, expiresAt, personalMessage }) {
  const renderActionsAsMarkdown = requiresMarkdownDeliveryActions(qurlLink);
  const embed = buildDeliveryEmbed({
    senderAlias, guildName, guildIconUrl, qurlLink, expiresAt, personalMessage,
    renderActionsAsMarkdown,
  });
  const components = renderActionsAsMarkdown
    ? []
    : [new ActionRowBuilder().addComponents(buildStepThroughButton(qurlLink), buildTrustButton())];
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
// store ignores status writes for missing recipient rows. Persisting
// first keeps valid delivery status from being dropped. executeSendPipeline
// + handleAddRecipients both honor this — recordQURLSendBatch awaits
// completion (with an early-
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

  let viewed = 0;
  let displayedFloor = 0;
  let renderedGeneration = 0;
  let lastSourceSnapshotAt = null;
  let aggregateUsable = true;
  let lastStrongFinalCount = null;
  let allDone = false;

  // A tick that began before addRecipients must not render its stale view map.
  let trackingGeneration = 0;

  const control = {
    // Keep the status map and tracked IDs aligned for added recipients.
    addRecipients(count, newLinks) {
      // Ignore additions after stop; they would retain linkStatus and safeEdit until process exit.
      if (stopped) return;
      expectedCount += count;
      if (Array.isArray(newLinks)) {
        for (const item of newLinks) {
          const qid = item && item.qurlId;
          if (!qid) {
            // Keep the monitor degraded if a later recipient has no qurl_id.
            if (!viewCounterDegraded) {
              logger.warn('Monitor view counter degraded mid-life — addRecipients link missing qurl_id', { sendId });
            }
            viewCounterDegraded = true;
            continue;
          }
          if (!trackedQurlIds.has(qid)) {
            trackedQurlIds.add(qid);
            linkStatus.set(qid, { status: 'pending', username: item.username || 'unknown' });
          }
        }
      }
      trackingGeneration++;
      lastSourceSnapshotAt = null;
      // Re-enter the dense polling phase without extending monitor lifetime.
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

  // No explicit `expired` state in the rendered counter: qurl.accessed
  // is the only event we receive. The interaction-token edit window
  // bounds the 14-min monitor cap (#465). Pre-PR-455 modelled
  // expired explicitly via upstream polling; the webhook-only world
  // can't observe expiration without a separate qurl.expired
  // subscription (out of scope).
  // Delegates to the pure renderViewCounter so the monitor's render and
  // PR-B's off-monitor fast-path render stay byte-identical. The closure
  // still owns the LIVE inputs (currentBaseMsg/viewed/expectedCount
  // mutate across ticks + addRecipients; viewCounterDegraded can flip
  // mid-life) — buildStatusMsg snapshots them at call time and hands them
  // to the pure fn.
  function buildStatusMsg(viewedOverride = Math.max(viewed, displayedFloor)) {
    return renderViewCounter({
      baseMsg: currentBaseMsg,
      viewed: viewedOverride,
      expectedCount,
      degraded: viewCounterDegraded,
    });
  }

  // Aggregate reads stay an optimization: reconcile source facts at least every
  // 30s during dense polling, and each steady tick whose interval exceeds 30s.
  // This repairs a crash between source recording and the non-atomic shard ADD.
  const SOURCE_RECONCILE_MS = 30_000;
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
      const useAggregate = trackedQurlIds.size > 100 && aggregateUsable;
      let floor = null;
      const readFloor = async () => {
        try { floor = await db.getSendRenderedCount(sendId); }
        catch (err) { logger.debug('Monitor floor-read failed; rendering local poll count', { sendId, error: err.message }); }
        if (Number.isSafeInteger(floor) && floor >= 0) displayedFloor = Math.max(displayedFloor, floor);
      };
      if (useAggregate) await readFloor();
      if (trackingGeneration !== genAtStart || !interaction) return;
      const haveFloor = Number.isSafeInteger(floor) && floor >= 0;
      const age = lastSourceSnapshotAt === null ? -1 : Date.now() - lastSourceSnapshotAt;
      let aggregate = null;
      if (useAggregate && haveFloor && age >= 0 && age < SOURCE_RECONCILE_MS) {
        try {
          const count = await db.getSendViewedCount(sendId, expectedCount);
          if (Number.isSafeInteger(count) && count >= Math.max(viewed, displayedFloor)
              && count <= expectedCount) aggregate = count;
          else aggregateUsable = false; // A known gap must not add a failed aggregate read to every source poll.
        } catch (err) {
          logger.debug('Monitor aggregate unavailable; reconciling source', { sendId, error: err.message });
        }
      }
      if (trackingGeneration !== genAtStart || !interaction) return;
      // Only source facts can settle the monitor. Final reads are strong so a
      // committed first-view fact cannot lag its aggregate and strand the loop.
      const finalCandidate = Math.max(aggregate ?? 0, displayedFloor);
      const verifyFinal = finalCandidate >= expectedCount && finalCandidate !== lastStrongFinalCount;
      if (aggregate === null || verifyFinal) {
        const views = await db.getQurlViews([...trackedQurlIds], { consistentRead: verifyFinal });
        if (trackingGeneration !== genAtStart || !interaction) return;
        for (const [qurlId, view] of views) {
          if (!(view.accessCount > 0)) continue;
          const current = linkStatus.get(qurlId);
          if (!current || current.status === 'opened') continue;
          linkStatus.set(qurlId, { ...current, status: 'opened' });
          viewed++;
        }
        lastSourceSnapshotAt = Date.now();
        if (verifyFinal) lastStrongFinalCount = finalCandidate;
        if (verifyFinal && viewed < expectedCount) {
          aggregateUsable = false;
          aggregate = null;
        }
      }
      let rendered = Math.max(viewed, displayedFloor, aggregate ?? 0);
      if (rendered <= displayedFloor && renderedGeneration === trackingGeneration) {
        if (viewed >= expectedCount) { allDone = true; clearTimeout(timer); }
        return;
      }
      // Small sends retain one source BatchGet per unchanged tick, without extra floor/aggregate reads.
      if (!useAggregate) await readFloor();
      if (trackingGeneration !== genAtStart || !interaction) return;
      rendered = Math.max(rendered, displayedFloor);
      if (rendered <= displayedFloor && renderedGeneration === trackingGeneration) {
        if (viewed >= expectedCount) { allDone = true; clearTimeout(timer); }
        return;
      }
      const pending = Math.max(0, expectedCount - rendered);
      const ok = await safeEdit({ content: buildStatusMsg(rendered), components: pending > 0 ? [buttonRow] : [] });
      if (!ok || !interaction || stopped) return; // Never commit a failed or stopped render.
      displayedFloor = Math.max(displayedFloor, rendered);
      renderedGeneration = genAtStart;
      if (viewed >= expectedCount && trackingGeneration === genAtStart) { allDone = true; clearTimeout(timer); }
      try { await db.tryAdvanceRenderedCount(sendId, rendered); }
      catch (err) {
        logger.debug('Monitor floor-advance failed (non-CCFE); fast-path/poll self-heal on next tick', { sendId, error: err.message });
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

// Wait budget for best-effort revoke cleanup that sits in front of a user
// reply: mint-failure compensation (after the 100s mint budget and 70s inline
// partial cleanup) and Add Recipients fresh-mint cleanup.
// TODO(upstream-contract): keeps the reply inside Discord's 15-minute
// interaction-token window. Hitting it is expected while
// the SDK fallback is slow, so it logs a warning with the ids, not an error.
const CLEANUP_WAIT_BUDGET_MS = 120_000;
// TODO(upstream-contract): /qurl revoke result budget; 13 minutes leaves room
// to edit the result before Discord's 15-minute interaction token expires.
const REVOKE_SELECT_RESULT_WAIT_MS = 13 * 60 * 1000;
const REVOKE_WORK_DEADLINE_MS = REVOKE_SELECT_RESULT_WAIT_MS - 60_000;
const REVOKE_RUNNING_MSG = 'Revocation is still running. Run `/qurl revoke` again in a few minutes to check the result.';

/**
 * Mint bounded request batches against the uploaded resource.
 * TODO(upstream-contract): GET /v1/quota reports unlimited qURLs per resource.
 * Re-uploading identical content can return the same resource and cannot
 * reset an allowance.
 *
 * @param {object} opts
 * @param {string} opts.initialResourceId — resource_id from the upload
 * @param {string} opts.expiresAt — ISO string; forwarded to mintLinks.
 * @param {number} opts.recipientCount — number of tokens to mint in total.
 * @param {string} opts.apiKey — qURL API key.
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
 * @returns {Array<{qurl_link: string, qurl_id: string, expires_at: string, resourceId: string}>}
 * A short result has scheduled cleanup and must never be delivered.
 */
async function mintLinksInBatches({ sendId, initialResourceId, expiresAt, recipientCount, apiKey, selfDestructSeconds = null, guildId }) {
  const allLinks = [];

  // Ten is a request batch size, not a per-resource quota.
  try {
    for (let i = 0; i < recipientCount; i += MINT_BATCH_SIZE) {
      const batchSize = Math.min(MINT_BATCH_SIZE, recipientCount - i);
      const minted = await mintLinks(initialResourceId, {
        expiresAt,
        n: batchSize,
        apiKey,
        selfDestructSeconds,
        guildId,
      });
      // An untrusted over-count cannot drive unbounded compensation: push the
      // requested links plus a bounded overflow so small over-mints self-heal
      // in the catch below (the over-count is thrown on after this loop).
      for (const link of minted.slice(0, batchSize + MAX_OVERFLOW_REVOKE_IDS)) {
        allLinks.push({ qurl_link: link?.qurl_link, qurl_id: link?.qurl_id, expires_at: link?.expires_at, resourceId: initialResourceId });
      }
      // Validate only after every entry is pushed: the catch below revokes the
      // identified siblings of a bad entry. More links than requested breaks
      // the mint contract. A short batch is still reported by callers as
      // "Only N of M"; links carry no recipient identity, so a short batch
      // cannot misroute access.
      if (minted.length > batchSize) {
        // Up to MAX_OVERFLOW_REVOKE_IDS extra children are revoked by the catch;
        // anything beyond is left for hand reconciliation, so log a bounded
        // slice of those non-secret ids.
        logger.error('Connector mint_link over-minted', {
          resource_ref: resourceIdLogRef(initialResourceId),
          requested: batchSize,
          returned: minted.length,
          unrevoked_overflow_qurl_ids: minted
            .slice(batchSize + MAX_OVERFLOW_REVOKE_IDS, batchSize + 2 * MAX_OVERFLOW_REVOKE_IDS)
            .map(link => qurlIdForCleanup(link?.qurl_id)).filter(id => id !== null),
        });
        throw new Error(`Connector mint_link returned ${minted.length} links for a ${batchSize}-link batch`);
      }
      minted.forEach((link, idx) => {
        // qurl_id is the only durable child-revoke identity (and the join key
        // against qurl.accessed webhooks). Never persist or deliver without it.
        if (!hasPersistableQurlIdShape(link?.qurl_id)) {
          throw new Error(`Connector mint_link returned a link without a valid qurl_id (entry ${i + idx})`);
        }
        // qurl_link is the write-once delivery credential; an id-only 2xx entry
        // can be neither delivered nor rebuilt, so revoke it rather than persist.
        if (typeof link.qurl_link !== 'string' || link.qurl_link.length === 0) {
          throw new Error(`Connector mint_link returned a link without a qurl_link (entry ${i + idx})`);
        }
        // TODO(upstream-contract): every deliverable mint carries its enforced expiry.
        const expiryMs = typeof link.expires_at === 'string' ? Date.parse(link.expires_at) : NaN;
        if (!Number.isFinite(expiryMs) || expiryMs <= Date.now()) {
          throw new Error(`Connector mint_link returned a link without a valid future expiry (entry ${i + idx})`);
        }
      });
      if (minted.length < batchSize) break;
    }
  } catch (error) {
    // Earlier batches' links were never delivered; revoke them (and the
    // identified part of this batch) before rethrowing so a failed mint does
    // not strand live children. mintLinks already revoked its own non-2xx
    // partial ids. Cleanup failures are logged, never mask the mint error.
    const idsByResource = new Map();
    let unidentifiedCount = 0;
    for (const link of allLinks) {
      const qurlId = qurlIdForCleanup(link.qurl_id);
      if (qurlId === null) {
        unidentifiedCount++;
        continue;
      }
      const ids = idsByResource.get(link.resourceId) || [];
      ids.push(qurlId);
      idsByResource.set(link.resourceId, ids);
    }
    const entries = cleanupBatchesByResource(idsByResource);
    const compensation = batchSettled(entries, ([resourceId, ids]) => (
      revokeMintedLinks(resourceId, ids, apiKey)
    ), 5);
    // Stop waiting before the send's interaction token expires so the mint
    // error still reaches the user; nothing was delivered, and the ids are
    // logged for reconciliation while the revoke keeps running.
    if (!await settlesWithin(compensation, CLEANUP_WAIT_BUDGET_MS)) {
      logger.warn('Mint failure compensation still running at its wait budget', {
        resources: entries.map(([resourceId, ids]) => ({ resource_ref: resourceIdLogRef(resourceId), qurl_ids: ids })),
        unidentified_count: unidentifiedCount,
      });
      throw error;
    }
    const results = await compensation;
    // qurl_ids are non-secret revoke handles: log them so an operator can
    // reconcile any child whose compensation failed.
    const failures = results.flatMap((result, i) => (result.status === 'rejected' ? [{
      resource_ref: resourceIdLogRef(entries[i][0]),
      qurl_ids: entries[i][1],
      error: result.reason?.message,
    }] : []));
    if (failures.length > 0 || unidentifiedCount > 0) {
      logger.error('Failed to revoke links after a mint failure', {
        failed_count: failures.length,
        total: results.length,
        unidentified_count: unidentifiedCount,
        failures,
      });
    }
    throw error;
  }
  if (allLinks.length < recipientCount) {
    await cleanupFreshMintedLinks(allLinks.map(link => ({
      resourceId: link.resourceId, qurlId: link.qurl_id,
    })), apiKey, sendId, { rowsMayHavePersisted: false, reason: 'short_mint' });
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
//   - `expiresIn` must be a current choice or the saved legacy 7d preset.
//   - `personalMessage` must be `null` or `string`.
//   - `recipients` must be a non-empty, capped array of objects with
//     a non-empty string id. Optional username fields are caller-provided.
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

// Best-effort scrub for persistence diagnostics: every HTTP(S) URL is removed,
// and scheme-less access tokens using qurl-service's current prefix are removed.
// TODO(upstream-contract): qurl-service owns the `at_` access-token prefix.
// Other scheme-less credential formats are intentionally outside this narrow
// helper; callers must still omit raw bearer fields, while the structured
// logger redacts metadata whose key is shaped like a qURL access link.
function scrubQurlCredentialForLog(message) {
  if (typeof message !== 'string') return undefined;
  return truncForLog(message
    .replace(/https?:\/\/\S+/gi, '[REDACTED_URL]')
    .replace(/(?<![A-Za-z_])at(?:_|%5f)[A-Za-z0-9_%-]+/gi, 'at_[REDACTED]'));
}

function persistenceErrorMessageForLog(err) {
  if (typeof err?.message === 'string') return scrubQurlCredentialForLog(err.message);
  if (err?.message != null) return scrubQurlCredentialForLog(String(err.message));
  if (typeof err !== 'object' || err == null) return scrubQurlCredentialForLog(String(err));
  return undefined;
}

async function executeSendPipeline(interaction, {
  apiKey,
  resourceType,
  attachment,
  locationUrl,
  locationName,
  // Initial recipients are deduped and bot-filtered upstream; self-send is allowed.
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
  const confirmationNonce = typeof sendNonce === 'string' && /^[a-f0-9]{16}$/.test(sendNonce)
    ? sendNonce : undefined;
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
  if (!isValidSavedExpiry(expiresIn)) {
    failGate(TypeError, `executeSendPipeline: expiresIn must be one of ${[...Object.keys(EXPIRY_LABELS), '7d (legacy)'].join('|')} (got ${truncForLog(expiresIn)})`);
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
  for (const [index, recipient] of recipients.entries()) {
    if (!recipient || typeof recipient !== 'object' || Array.isArray(recipient)
        || typeof recipient.id !== 'string' || !recipient.id.trim()) {
      failGate(TypeError, `executeSendPipeline: recipients[${index}] must be an object with a non-empty string id`);
    }
  }
  // Add Recipients owns this snapshot; callers may retain or freeze their input.
  recipients = [...recipients];

  // Operator-visibility hook for big sends. Fires when a single send
  // crosses `largeSendThreshold()` so the operational cost (qurl-
  // service mint batches + DM fan-out duration at Discord's per-bot
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
        logger.audit(AUDIT_EVENTS.UPLOAD_REJECTED_CAPACITY, {
          active_file_sends: activeFileSends, max_concurrent: MAX_CONCURRENT_FILE_SENDS,
        });
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

      const { resource_id: resourceId } = await downloadAndUpload(attachment.url, filename, attachment.contentType, apiKey, selfDestructSeconds);
      connectorResourceId = resourceId;
      const allLinks = await mintLinksInBatches({
        sendId,
        initialResourceId: resourceId,
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

      if (allLinks.length < recipients.length) {
        logger.error('mintLinks returned fewer links than expected', { expected: recipients.length, got: allLinks.length });
        clearCooldown(interaction.user.id);
        return interaction.editReply({ content: `Only ${allLinks.length} of ${recipients.length} links could be created. Please try again.` });
      }

      qurlLinks = recipients.map((r, i) => ({
        recipientId: r.id,
        qurlLink: allLinks[i].qurl_link,
        expiresAt: Math.floor(Date.parse(allLinks[i].expires_at) / 1000),
        qurlId: allLinks[i].qurl_id,
        resourceId: allLinks[i].resourceId,
      }));
      logger.audit(AUDIT_EVENTS.UPLOAD_SUCCESS, { send_id: sendId, kind: 'file' });
    } else {
      // Location send — upload JSON payload to connector, then mint in batches
      // of MINT_BATCH_SIZE against the same resource.
      const locPayload = { type: 'google-map', url: locationUrl, name: locationName || locationUrl };
      // Note: google-map JSON resources hit the connector's render
      // carve-out (mapEmbedTmpl/mapFallbackTmpl don't honor
      // expire_after at view time — infra repo #480).
      // We still forward selfDestructSeconds so behavior matches the
      // contract once the carve-out is removed; today it's a no-op.
      const firstUpload = await uploadJsonToConnector(locPayload, 'location.json', apiKey, selfDestructSeconds);
      connectorResourceId = firstUpload.resource_id;

      const expiresAt = expiryToISO(expiresIn);
      const allLinks = await mintLinksInBatches({
        sendId,
        initialResourceId: firstUpload.resource_id,
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
        expiresAt: Math.floor(Date.parse(allLinks[i].expires_at) / 1000),
        qurlId: allLinks[i].qurl_id,
        resourceId: allLinks[i].resourceId,
      }));
      logger.audit(AUDIT_EVENTS.UPLOAD_SUCCESS, { send_id: sendId, kind: 'location' });
    }
  } catch (error) {
    // Audit for the CloudWatch metric filter + alarm at the infrastructure repository
    // qurl-bot-discord/terraform/monitoring.tf (qurl-integrations#276); the why
    // lives in the QURL_SEND_CREATE_LINK_FAILURE docstring in constants.js.
    // kindMap (not a `=== FILE ? 'file' : 'location'` ternary) so a future third
    // RESOURCE_TYPES surfaces as `null` — discoverable in CloudWatch — rather than
    // silently bucketing as 'location'. quota_exceeded skip lives in emitMintFailureAudit.
    const kindMap = { [RESOURCE_TYPES.FILE]: 'file', [RESOURCE_TYPES.MAPS]: 'location' };
    emitMintFailureAudit(error, { sendId, kind: kindMap[resourceType] ?? null });
    logger.error('Failed to prepare qURL links', {
      error: error.message,
      apiCode: error.apiCode,
      status: error.status,
      ...logSafeApiDetail(error),
      ...(error.partialLinkCount ? {
        partial_link_count: error.partialLinkCount,
        partial_qurl_ids: error.partialQurlIds,
      } : {}),
      sendId,
    });
    clearCooldown(interaction.user.id); // allow retry on failure
    // Slot release lives in the `finally` block below — it ALWAYS runs
    // after a return-from-catch, and `releaseSlot` is idempotent via
    // the `fileSendSlotClaimed` flag. Dropping the duplicate call here
    // keeps the single-release-path invariant visible at a glance.
    if (error.apiCode === 'quota_exceeded') {
      return interaction.editReply({
        content: "Couldn't create links because an account limit was reached. Check your qURL account usage before trying again.",
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

  // Per-link mint expiries were validated before any recipient row is written.

  // Persist ALL links to DB BEFORE sending DMs. If the write fails the links
  // still exist on the qURL side but there's no local record to revoke them
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
      expiresAt: link.expiresAt,
      // CONTRACT: targetType is always 'user' for new rows post-PR
      // #313 (the only caller is executeSendPipeline via the confirm
      // card on /qurl send + /qurl map, both of which DM individual
      // recipients). The column is kept because the revoke-list
      // renderer's branch on `s.target_type` still has to handle
      // historical legacy /qurl send wizard rows ('channel') during the TTL drain
      // window. #318 drops the formatRevokeLabel non-'user' branch
      // once no revoke-visible row has `target_type !== 'user'` — the
      // drain requires a check of stored legacy rows before retiring
      // the reader; seven-day requested windows do not prove actual expiry.
      // No entry-gate fires because the
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
    // Log only non-secret identifiers so an operator can manually revoke the
    // orphaned qURLs. qurlLink carries its live access token in the fragment
    // and must never reach logs. resourceId + qurlId are the identities the
    // bot's child revoke (revokeMintedLinks) needs to reconcile each orphan.
    // Scrub err.message even for AWS service exceptions: validation messages
    // can echo offending request values, but contain useful failure details.
    logger.error('recordQURLSendBatch failed; aborting send to keep state consistent', {
      sendId,
      errorName: err?.name,
      errorCode: err?.code,
      errorFault: err?.$fault,
      httpStatusCode: err?.$metadata?.httpStatusCode,
      requestId: err?.$metadata?.requestId,
      errorMessage: persistenceErrorMessageForLog(err),
      linkCount: qurlLinks.length,
      orphanedResources: qurlLinks.map(l => ({ resourceId: l.resourceId, qurlId: l.qurlId })),
    });
    await cleanupFreshMintedLinks(qurlLinks, apiKey, sendId, {
      reason: 'persistence_failure',
    });
    clearCooldown(interaction.user.id);
    return interaction.editReply({
      content: 'Failed to save link records. Links were not sent. Please try again.',
    });
  }

  // Send only links that remain live at dispatch time.
  let delivered = 0;
  let failed = 0;
  const failedUsers = [];
  const recipientMap = new Map(recipients.map(r => [r.id, r]));

  // Resolve shared sender provenance once; expiry remains per-link.
  const senderAlias = resolveSenderAlias(interaction);
  const { guildName, guildIconUrl } = resolveGuildProvenance(interaction);

  const dmResults = await batchSettled(qurlLinks, async (link) => {
    const recipient = recipientMap.get(link.recipientId);
    // Audit payload and transport failures before the status write so a
    // bookkeeping failure cannot hide the dispatch outcome.
    let result = { ok: false };
    try {
      if (link.expiresAt <= Math.floor(Date.now() / 1000)) throw new Error('Minted link expired before delivery');
      const dmPayload = buildDeliveryPayload({
        senderAlias,
        guildName,
        guildIconUrl,
        qurlLink: link.qurlLink,
        expiresAt: link.expiresAt,
        personalMessage,
      });
      result = await sendDM(link.recipientId, dmPayload);
    } catch (err) {
      logger.warn('Recipient delivery failed before completion', { sendId, errorName: err?.name ?? 'NonError' });
    } finally {
      logger.audit(result.ok === true ? AUDIT_EVENTS.DISPATCH_SENT : AUDIT_EVENTS.DISPATCH_FAILED, { send_id: sendId, send_nonce: confirmationNonce });
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
  // adding recipients.
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
  // re-uploading. Add Recipients labels the retained file as the
  // original list; the post-revoke list includes later recipients.
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
  const interactionCreatedAt = Number.isFinite(interaction.createdTimestamp)
    ? Math.min(Date.now(), interaction.createdTimestamp)
    : 0;
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
        // Token expiry is measured from interaction creation, including time spent minting.
        // Unknown creation time expires closed; DDB self-defense then hides the bearer token.
        confirmExpiresAt: Math.floor((interactionCreatedAt + TIMEOUTS.QURL_REVOKE_WINDOW) / 1000),
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
    const notifyMsg = `📩 **${safeName}** shared something with ${audience} via **qURL**. Check your DMs.`;
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
      // Discord tokens age from creation, not from when a slow send finishes.
      // Leave 10s for the bounded state read and final edit; unknown age closes promptly.
      const tokenAgeMs = Date.now() - interactionCreatedAt;
      collector = response.createMessageComponentCollector({
        filter: (i) => i.user.id === interaction.user.id,
        time: Math.max(1, TIMEOUTS.QURL_REVOKE_WINDOW - tokenAgeMs - 10_000),
      });
    } catch (err) {
      logger.error('Failed to create button collector', { sendId, error: err.message });
      monitor.stop();
      return;
    }

    const revokeLockKey = `${interaction.user.id}:${sendId}`;
    let showAllRecipients = false;

    // `revokeInFlight` dedups concurrent Revoke clicks. `revokeSucceeded`
    // guards the on('end') re-render so a Failed message isn't overwritten
    // by a stale "Revoked 0/0". These flags are collector-local UX gates;
    // revokingSendLocks handles same-process cross-collector Revoke only
    // while work is active. After the lock releases, and across processes,
    // revoking_at/revoked_at are the correctness boundary;
    // recordQURLSendBatch enforces them again in the same transaction as any
    // later Add Recipients rows.
    let revokeResultUserNames = [];
    let revokeResultTotal = 0;
    // Authoritative DDB strict-success count. Tracked separately from
    // `revokeResultUserNames.length` so the header stays correct even
    // if a successful recipient_id can't be name-resolved against
    // `recipients[]`.
    let revokeResultSuccess = 0;
    let revokeResultFinalizationFailed = false;
    let revokeShowAll = false;
    let revokeInFlight = false;
    let revokeSucceeded = false;
    let revokeAttempted = false;
    let revokeResultKnown = false;

    collector.on('collect', async (btnInteraction) => {
      if (btnInteraction.customId === `qurl_expand_${sendId}`) {
        await btnInteraction.deferUpdate().catch(logIgnoredDiscordErr);
        // The result card has a separate qurl_revoke_expand action.
        if (revokeAttempted) return;
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
        const updated = renderRevokeMsg(
          sendId,
          revokeResultUserNames,
          revokeResultTotal,
          revokeShowAll,
          revokeResultSuccess,
          revokeResultFinalizationFailed,
        );
        await btnInteraction.editReply(revokeReplyPayload(updated)).catch(logIgnoredDiscordErr);
        return;
      }

      if (btnInteraction.customId === `qurl_revoke_${sendId}`) {
        // Sync dedup before any await (Node single-threaded).
        if (revokeInFlight) return btnInteraction.deferUpdate().catch(logIgnoredDiscordErr);
        if (revokingSendLocks.has(revokeLockKey)) {
          await btnInteraction.reply({ content: ALREADY_REVOKING_SEND_MSG, flags: MessageFlags.Ephemeral }).catch(logIgnoredDiscordErr);
          return;
        }
        if (addRecipientsLocks.has(sendId)) {
          await btnInteraction.reply({ content: `${ADD_RECIPIENTS_IN_PROGRESS_MSG} Finish the current selection or try again in a moment.`, flags: MessageFlags.Ephemeral }).catch(logIgnoredDiscordErr);
          return;
        }
        revokeInFlight = true;
        revokingSendLocks.add(revokeLockKey);
        // Keep this lock owned by the revoke work, not the collector lifetime:
        // if delete I/O hangs, Add stays blocked until that work settles (or
        // the process restarts) rather than minting while revoke may still run.
        // Stop monitor BEFORE any editReply — its poll loop can
        // overwrite the revoke-result message otherwise. Bare call
        // (no `if (monitor)`) — we're inside the `if (monitor) { ... }`
        // collector-setup block; the guard above already proved truthy.
        let revoking;
        try {
          revokeAttempted = true;
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
            revokeResultFinalizationFailed = false;
            revokeShowAll = false;
            revokeResultKnown = false;
            // Keep revokeInFlight true after success as the collector-local
            // terminal gate for duplicate Revoke clicks; revokingSendLocks only
            // covers in-progress work across same-process collectors.
            revokeSucceeded = true;
            await btnInteraction.editReply({ content: 'Links for this send have already been revoked.', components: [] }).catch(logIgnoredDiscordErr);
            return;
          }
          await btnInteraction.editReply({ content: 'Revoking links...', components: [] }).catch(logIgnoredDiscordErr);
          revoking = revokeAllLinks(sendId, interaction.user.id, apiKey, resolveSenderAlias(interaction))
            .finally(() => revokingSendLocks.delete(revokeLockKey));
          if (!await settlesWithin(revoking, REVOKE_SELECT_RESULT_WAIT_MS)) {
            logger.warn('Revoke button still running at its result budget', { sendId });
            await btnInteraction.editReply({ content: REVOKE_RUNNING_MSG, components: [] }).catch(logIgnoredDiscordErr);
            return;
          }
          const revoked = await revoking;
          if (!revoked.barrierEstablished) {
            revokeResultUserNames = [];
            revokeResultTotal = 0;
            revokeResultSuccess = 0;
            revokeResultFinalizationFailed = false;
            revokeShowAll = false;
            revokeResultKnown = false;
            // Terminal for this collector even though the exact reason is
            // intentionally hidden (unknown, foreign, or concurrently
            // finalized all share the store's fail-closed result).
            revokeSucceeded = true;
            await btnInteraction.editReply({
              content: 'Could not verify this send for revocation. It may already be revoked or unavailable; run `/qurl revoke` to refresh.',
              components: [],
            }).catch(logIgnoredDiscordErr);
            return;
          }
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
          revokeResultFinalizationFailed = revoked.finalizationFailed;
          revokeShowAll = false;
          revokeResultKnown = true;
          const initial = renderRevokeMsg(
            sendId,
            revokeResultUserNames,
            revokeResultTotal,
            false,
            revokeResultSuccess,
            revokeResultFinalizationFailed,
          );
          await btnInteraction.editReply(revokeReplyPayload(initial)).catch(logIgnoredDiscordErr);
          // Keep revokeInFlight true after success as the collector-local
          // terminal gate for duplicate Revoke clicks; revokingSendLocks only
          // covers in-progress work across same-process collectors.
          revokeSucceeded = true;
          // Freeze the confirmation display so a late webhook fast-path
          // edit can't resurrect a live-looking "👀 N viewed" counter
          // over this "Revoked X/Y" terminal copy (getSendRenderState
          // reads the persisted fast-path-off flag, which the fast-path checks
          // FIRST and skips on). Best-effort + logged-swallowed: a miss
          // narrows to a cosmetic race (a view landing in the tens-of-ms
          // before the flag lands could re-render the counter once); the
          // monitor.stop() above already halts the poll, so this is the
          // only remaining off-monitor editor to fence.
          db.markConfirmFastPathOff(sendId).catch((err) => {
            logger.warn('markConfirmFastPathOff (revoke) failed; a late fast-path edit could briefly re-render the counter', { sendId, error: err.message });
          });
        } catch (err) {
          logger.error('Revoke failed', { sendId, error: err.message });
          await btnInteraction.editReply({
            content: 'Failed to revoke links. Try `/qurl revoke` instead.',
            components: [],
          }).catch(logIgnoredDiscordErr);
          // Release the in-process gate; retry from /qurl revoke after this card closes.
          // Durable revoking_at also protects other collectors/processes.
          revokeInFlight = false;
        } finally {
          if (!revoking) revokingSendLocks.delete(revokeLockKey);
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
        if (revokingSendLocks.has(revokeLockKey) || revokeSucceeded) {
          // Durable revoking_at/revoked_at state keeps stale Add clicks
          // disabled even after this collector-local lock is released.
          let content = ALREADY_REVOKING_SEND_MSG;
          if (revokeSucceeded) {
            if (!revokeResultKnown) {
              content = 'This send is no longer revocable. Add Recipients is disabled.';
            } else if (revokeResultFinalizationFailed) {
              content = 'Links were revoked, but qURL could not save the final state. Add Recipients is disabled; retry `/qurl revoke`.';
            } else if (revokeResultTotal === 0) {
              content = 'No live links remain for this send.';
            } else if (revokeResultSuccess < revokeResultTotal) {
              content = 'Revocation is incomplete for this send. Add Recipients is disabled; retry `/qurl revoke`.';
            } else {
              content = 'Links for this send have already been revoked.';
            }
          }
          await btnInteraction.reply({ content, flags: MessageFlags.Ephemeral }).catch(logIgnoredDiscordErr);
          return;
        }
        if (revokeAttempted) {
          await btnInteraction.reply({
            content: 'This management card is closed after a revoke attempt. Run `/qurl revoke` to check the result.',
            flags: MessageFlags.Ephemeral,
          }).catch(logIgnoredDiscordErr);
          return;
        }
        if (addRecipientsLocks.has(sendId)) {
          await btnInteraction.reply({ content: `${ADD_RECIPIENTS_IN_PROGRESS_MSG} Finish the current selection or try again in a moment.`, flags: MessageFlags.Ephemeral }).catch(logIgnoredDiscordErr);
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
              flags: MessageFlags.Ephemeral,
            }).catch(logIgnoredDiscordErr);
            return;
          }
          if (isOnCooldown(interaction.user.id)) {
            await btnInteraction.reply({ content: 'Please wait before adding more recipients.', flags: MessageFlags.Ephemeral }).catch(logIgnoredDiscordErr);
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
          await btnInteraction.reply({
            content: 'Select additional recipients:',
            components: [userSelectRow],
            flags: MessageFlags.Ephemeral,
          });
          const selectReply = await btnInteraction.fetchReply();

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
              confirmMsg = `Sent to ${totalSent} user${totalSent !== 1 ? 's' : ''} | Requested expiry: ${expiresIn} | ${formatSelfDestructSegment(selfDestructSeconds)}`;
              if (failed > 0) confirmMsg += `\n${failed} could not be reached`;
              if (confirmRendered.attachmentText) {
                confirmMsg += `\nAttached recipients.txt lists only the original recipients; newly added recipients are not listed.`;
              }
              monitor.updateBaseMsg(confirmMsg);
              await interaction.editReply({ content: monitor.getFullMsg(), components: [buttonRow] });
              // Mid-life degrade: an added link missing its qurl_id flips
              // the send to view-counter-degraded (the monitor renders bare
              // baseMsg from here on). The fast-path can't see that, so
              // DISARM it by marking the confirmation terminal — its
              // fast-path-off guard then skips, leaving the (bare) poll render
              // as the sole renderer. Matches the send-time degraded gate.
              if ((addResult.newLinks || []).some(l => !l?.qurlId)) {
                db.markConfirmFastPathOff(sendId).catch((err) => {
                  logger.warn('markConfirmFastPathOff (add-recipients degrade) failed; fast-path could briefly stamp a partial counter until poll re-renders bare', { sendId, error: err.message });
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

    // EventEmitter ignores listener promises; consume every close-path failure.
    collector.on('end', (_, reason) => (async () => {
      // stop() freezes the final getFullMsg() render internally, so the
      // post-stop getFullMsg() below preserves the last-known counter
      // (vs. re-rendering against a cleared linkStatus → `0 of N viewed`).
      monitor.stop();
      if (reason === 'time') {
        let revokedElsewhere = false;
        if (!revokeSucceeded && !revokeInFlight) {
          try {
            const persisted = await db.getSendConfig(sendId, interaction.user.id, { signal: AbortSignal.timeout(1000) });
            revokedElsewhere = Boolean(persisted?.revoked_at);
          } catch (err) {
            logger.warn('Could not check send revoked state when management closed', { sendId, error: err.message });
          }
        }
        // Re-check the local result after the read: a button revoke may have completed meanwhile.
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
              content: 'This send is no longer revocable.',
              components: [],
            }).catch(logIgnoredDiscordErr);
            return;
          }
          // Terminal state: re-render content (Show Recipients may have
          // toggled), strip components. Omit `files`/`attachments`
          // so Discord keeps the existing revoked-users.txt without
          // re-uploading the same blob 15min later.
          const final = renderRevokeMsg(
            sendId,
            revokeResultUserNames,
            revokeResultTotal,
            revokeShowAll,
            revokeResultSuccess,
            revokeResultFinalizationFailed,
          );
          interaction.editReply({ content: final.content, components: [] }).catch(logIgnoredDiscordErr);
          return;
        }
        // An active local revoke owns its final reply.
        if (revokeInFlight) return;
        // Persisted success wins after a failed attempt; revoked_at also fences refreshed webhook renders.
        if (revokedElsewhere) {
          interaction.editReply({ content: 'Links for this send have already been revoked.', components: [] }).catch(logIgnoredDiscordErr);
          return;
        }
        // Management window closed — this is the confirmation's terminal
        // render. Freeze it so a webhook fast-path edit arriving after the
        // collector ended can't re-animate the counter over the
        // window-closed banner. Fire-and-forget .catch in this 'end'
        // handler (best-effort, logged-swallowed); the monitor.stop()
        // above froze the poll, leaving the fast-path as the only
        // off-monitor editor to fence. Expired is intentionally NOT fenced
        // here — the qurl.expired handler edits the recipient DM, never
        // this sender confirmation, so it needs no terminal mark.
        db.markConfirmFastPathOff(sendId).catch((err) => {
          logger.warn('markConfirmFastPathOff (window-closed) failed; a late fast-path edit could briefly re-render the counter', { sendId, error: err.message });
        });
        interaction.editReply({
          content: monitor.getFullMsg() + '\n\n⏰ **Management window closed** — use `/qurl revoke` to revoke later.',
          components: [],
        }).catch(logIgnoredDiscordErr);
      }
    })().catch(err => {
      logger.error('Failed to close send management view', { sendId, errorName: err?.name });
    }));
  }
}

// Keep cleanup failures isolated to one transport-sized batch, even under one parent.
// Each job owns its route probe; a failed job cannot skip later children.
function cleanupBatchesByResource(idsByResource) {
  const batches = [];
  for (const [resourceId, ids] of idsByResource) {
    for (let offset = 0; offset < ids.length; offset += REVOKE_BATCH_MAX_IDS) {
      batches.push([resourceId, ids.slice(offset, offset + REVOKE_BATCH_MAX_IDS)]);
    }
  }
  return batches;
}

async function cleanupFreshMintedLinks(batchSends, apiKey, sendId, options = {}) {
  const rowsMayHavePersisted = options.rowsMayHavePersisted !== false;
  const cleanupReason = options.reason || (rowsMayHavePersisted ? 'revoked_guard' : 'pre_persistence');
  // Initial sends may have partially committed BatchWrite chunks. Add
  // Recipients may have an ambiguous guarded transaction. No DMs have been
  // sent yet, so revoking the known fresh grants is the fail-closed outcome.
  const qurlIdsByResource = new Map();
  let unidentifiedCount = 0;
  for (const s of batchSends) {
    const qurlId = qurlIdForCleanup(s.qurlId);
    // Mint output is validated, so this is defensive: revoke identifiable
    // siblings instead of letting one bad row skip the whole resource, and
    // count a row missing either identity so the accounting is complete.
    if (typeof s.resourceId !== 'string' || s.resourceId.length === 0 || qurlId === null) {
      unidentifiedCount++;
      continue;
    }
    const ids = qurlIdsByResource.get(s.resourceId) || [];
    ids.push(qurlId);
    qurlIdsByResource.set(s.resourceId, ids);
  }
  const batches = cleanupBatchesByResource(qurlIdsByResource);
  if (batches.length === 0 && unidentifiedCount === 0) return;

  const cleanup = batchSettled(batches, async ([resourceId, ids]) => {
    await revokeMintedLinks(resourceId, ids, apiKey);
    return resourceId;
  }, 5);
  // Same bounded wait as mint-failure compensation: the send reply
  // must not be held past its interaction window; the revoke keeps running.
  if (!await settlesWithin(cleanup, CLEANUP_WAIT_BUDGET_MS)) {
    logger.warn('Send cleanup still running at its wait budget', {
      sendId,
      reason: cleanupReason,
      unidentified_count: unidentifiedCount,
      resources: batches.map(([resourceId, ids]) => ({
        resource_ref: resourceIdLogRef(resourceId),
        qurl_ids: ids,
      })),
    });
    return;
  }
  const results = await cleanup;
  const failed = [];
  results.forEach((result, index) => {
    if (result.status === 'rejected') {
      failed.push({
        resource_ref: resourceIdLogRef(batches[index][0]),
        qurl_ids: batches[index][1],
        error: result.reason?.message,
      });
    }
  });
  if (failed.length > 0 || unidentifiedCount > 0) {
    logger.error('Failed to clean up freshly minted qURL links', {
      sendId,
      reason: cleanupReason,
      failed_count: failed.length,
      total: batches.length,
      unidentified_count: unidentifiedCount,
      failures: failed,
    });
  } else {
    logger.info('Cleaned up freshly minted qURL links', {
      sendId,
      reason: cleanupReason,
      total: batches.length,
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

  // getSendConfig runs after the user-select await, so revoking_at/revoked_at
  // catch button, slash-command, and out-of-band revokes that landed while
  // the Add Recipients picker was open. recordQURLSendBatch repeats this
  // guard in the recipient-row transaction to close the post-read write
  // window.
  if (sendConfig.revoked_at) {
    return {
      msg: 'Cannot add recipients — this send has already been revoked.',
      newLinks: [], delivered: 0, failed: 0, newRecipients: [],
    };
  }
  if (sendConfig.revoking_at) {
    return {
      msg: 'Cannot add recipients — revocation is pending; retry `/qurl revoke`.',
      newLinks: [], delivered: 0, failed: 0, newRecipients: [],
    };
  }

  // #352: reject bad stored requests before expiryToISO can silently default.
  // Keep the retired 7d setting readable; actual link expiry comes from the mint.
  if (!isValidSavedExpiry(sendConfig.expires_in)) {
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
  // caller can extend its recipients[] after a successful add. Most
  // non-revoked failure paths return it for contract consistency; revoked
  // paths return [] so the post-revoke render cannot show phantom grants.
  const resolvedRecipients = newRecipients.map(u => ({ id: u.id, username: u.username }));

  if (newRecipients.length === 0) {
    return { msg: 'No valid recipients selected (bots and yourself are excluded).', newLinks: [], delivered: 0, failed: 0, newRecipients: resolvedRecipients };
  }

  // Create new qURL links for each resource type in the send config
  // recipientLinks[recipientId] = [{ qurlLink, resourceId, resType, label }]
  const recipientLinks = {};
  // connector_resource_id exists for file AND location sends because maps are
  // uploaded as JSON resources before minting. Classify by the persisted
  // payload shape instead, or a location send would look like a mixed send and
  // produce duplicate (send_id, recipient_id) rows.
  const hasFilePayload = Boolean(sendConfig.attachment_url);
  const hasLocation = Boolean(sendConfig.actual_url);
  if (hasFilePayload && hasLocation) {
    // Normal saveSendConfig callers persist one payload shape. Seeing both
    // means a stale/corrupt mixed row; reject it instead of minting duplicate
    // (send_id, recipient_id) rows that qurl_sends cannot represent cleanly.
    logger.warn('addRecipients refused mixed file/location send config', { sendId });
    return {
      msg: 'Cannot add recipients — mixed file and location sends are not supported. Create a new send instead.',
      newLinks: [], delivered: 0, failed: 0, newRecipients: [],
    };
  }
  if (hasFilePayload && sendConfig.resource_type && sendConfig.resource_type !== RESOURCE_TYPES.FILE) {
    logger.warn('addRecipients refused non-file send config with file payload', {
      sendId,
      resource_type: sendConfig.resource_type,
    });
    return {
      msg: 'Cannot add recipients — stored send configuration is unsupported. Create a new send instead.',
      newLinks: [], delivered: 0, failed: 0, newRecipients: [],
    };
  }
  const hasLegacyFileMarker = !sendConfig.resource_type
    && Boolean(sendConfig.connector_resource_id)
    && !hasLocation;
  const hasFile = hasFilePayload
    || ((sendConfig.resource_type === RESOURCE_TYPES.FILE || hasLegacyFileMarker) && !hasLocation);

  if (!hasFile && !hasLocation) {
    return { msg: 'Cannot add recipients — send configuration is incomplete.', newLinks: [], delivered: 0, failed: 0, newRecipients: resolvedRecipients };
  }

  // Tracks which prep path completed so upload_success can name the kind
  // after the shared try/catch. Mixed configs are rejected above because
  // qurl_sends cannot represent two rows for one send/recipient pair.
  const preparedKinds = [];
  // Inherit the original send's self-destruct timer so additional
  // recipients see the same vanish behavior. Persisted as a REAL/Number
  // column; both stores return null when unset. Hoisted above the file/
  // location branches because both pull the same value.
  const inheritedDestruct = sendConfig.self_destruct_seconds ?? null;
  // activeKind tracks which branch is in-flight when the outer catch
  // fires. A future refactor that throws before either branch sets it
  // lands kind=null, which is discoverable in CloudWatch.
  let activeKind = null;
  try {
    if (hasFile) {
      activeKind = 'file';
      // Resolve the retained attachment through the connector, then reuse
      // its returned resource for every mint batch.
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
      const filename = sendConfig.attachment_name || 'file';
      const contentType = sendConfig.attachment_content_type || 'application/octet-stream';

      try {
        // Upload once; subsequent batches use this same resource.
        const { resource_id: resourceId } = await downloadAndUpload(sendConfig.attachment_url, filename, contentType, apiKey, inheritedDestruct);
        allLinks = await mintLinksInBatches({
          sendId,
          initialResourceId: resourceId,
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
        // Only a permanent CDN response implies the source is unavailable.
        // Network, connector upload, and mint failures can be retried.
        const isExpired = [403, 404, 410].includes(err.cdnStatus);
        const msg = isExpired
          ? 'Original attachment is no longer available. Please create a new send.'
          : err.apiCode === 'quota_exceeded'
            ? 'An account limit was reached. Check your qURL account usage before adding recipients.'
            : 'Failed to prepare links. Please try again, or create a new send if the issue persists.';
        logger.error('addRecipients file re-upload failed', {
          sendId,
          error: err.message,
          apiCode: err.apiCode,
          status: err.status,
          ...logSafeApiDetail(err),
          ...(err.partialLinkCount ? {
            partial_link_count: err.partialLinkCount,
            partial_qurl_ids: err.partialQurlIds,
          } : {}),
          isExpired,
        });
        // Always emit — every failure here (CDN re-download, connector
        // re-upload, or mint) is a "couldn't create links" event. A rare,
        // expected CDN-expiry (Add Recipients on a >24h-old send) is absorbed
        // by the alarm's sustained threshold — no
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
          expiresAt: Math.floor(Date.parse(allLinks[i].expires_at) / 1000),
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
        sendId,
        initialResourceId: firstUpload.resource_id,
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
          expiresAt: Math.floor(Date.parse(allLinks[i].expires_at) / 1000),
          qurlId: allLinks[i].qurl_id,
          resourceId: allLinks[i].resourceId,
          resType: RESOURCE_TYPES.MAPS, label: sendConfig.location_name || 'Google Maps',
        });
      });
      preparedKinds.push('location');
    }
  } catch (error) {
    logger.error('Failed to create links for additional recipients', {
      sendId,
      error: error.message,
      apiCode: error.apiCode,
      status: error.status,
      ...logSafeApiDetail(error),
      ...(error.partialLinkCount ? {
        partial_link_count: error.partialLinkCount,
        partial_qurl_ids: error.partialQurlIds,
      } : {}),
    });
    const msg = error.apiCode === 'quota_exceeded'
      ? 'An account limit was reached. Check your qURL account usage before adding recipients.'
      : 'Failed to create links for new recipients.';
    // kind: activeKind — set on entry to each branch (see its decl above);
    // a future refactor that throws before either branch lands kind=null,
    // discoverable in CloudWatch. quota_exceeded skip lives in emitMintFailureAudit.
    emitMintFailureAudit(error, { sendId, kind: activeKind });
    return { msg, newLinks: [], delivered: 0, failed: 0, newRecipients: resolvedRecipients };
  }

  // Single emission per send. Values: 'file' | 'location'. The fallback is
  // defensive only; mixed configs are rejected before any prep path runs.
  if (preparedKinds.length > 0) {
    const kind = preparedKinds.length === 1 ? preparedKinds[0] : 'mixed';
    logger.audit(AUDIT_EVENTS.UPLOAD_SUCCESS, { send_id: sendId, kind });
  }

  const recipientIds = Object.keys(recipientLinks);
  if (recipientIds.length === 0) {
    return { msg: 'Failed to create any links.', newLinks: [], delivered: 0, failed: 0, newRecipients: resolvedRecipients };
  }

  // Provenance is shared; expiry comes from each minted link.
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
        expiresAt: link.expiresAt,
        expiresIn: sendConfig.expires_in,
        channelId: originalInteraction.channelId, targetType: 'user',
        // guild_id for /qurl detect attribution (#1101) — mirrors the mint
        // above + executeSendPipeline's row write. Reuses the original
        // send's guild so added recipients stay attributable. Sparse write.
        guildId: originalInteraction.guildId,
      });
    }
  }
  if (!ddbSendConfigGuardFitsTransaction(batchSends)) {
    logger.error('addRecipients refused oversized guarded write before DDB persistence', {
      sendId,
      send_count: batchSends.length,
      txn_actions: ddbSendConfigGuardActionCount(batchSends),
    });
    await cleanupFreshMintedLinks(batchSends, apiKey, sendId, {
      rowsMayHavePersisted: false,
      reason: 'pre_persistence_oversized_batch',
    });
    return {
      msg: 'Cannot add recipients — too many recipients selected. Try fewer recipients.',
      newLinks: [], newRecipients: [],
      delivered: 0,
      failed: 0,
    };
  }
  // Same dispatch guarantee as executeSendPipeline: if the DB write fails,
  // abort BEFORE any DMs go out. The revoked-race branch below also deletes
  // freshly minted resources because those rows deliberately never land.
  try {
    await db.recordQURLSendBatch(batchSends, { requireSendConfigUnrevoked: true });
  } catch (err) {
    if (err?.code === 'SEND_CONFIG_REVOKED') {
      logger.warn('recordQURLSendBatch refused Add Recipients for revoked send', {
        sendId, error: err.message, linkCount: batchSends.length,
      });
      await cleanupFreshMintedLinks(batchSends, apiKey, sendId);
      return {
        msg: 'Cannot add recipients — this send has already been revoked.',
        newLinks: [], newRecipients: [],
        delivered: 0,
        failed: 0,
      };
    }
    logger.error('recordQURLSendBatch failed in addRecipients; aborting before DMs', {
      sendId, error: err.message, linkCount: batchSends.length,
    });
    // Normal transaction failures are atomic, so no rows landed. If the final
    // retry actually committed but its response was lost, this cleanup can
    // leave rows pointing at deleted resources; that is still fail-closed
    // because no DMs were sent and the qURLs no longer grant access.
    await cleanupFreshMintedLinks(batchSends, apiKey, sendId, {
      reason: 'guarded_transaction_failed',
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
      // limit. Each embed names its link's host. Long qv2 links put
      // actions in the embed instead of Discord components.
      // The earlier empty-links guard keeps the bulk packer input nonempty.
      const cappedLinks = links.slice(0, 10);
      const renderActionsAsMarkdown = cappedLinks.some(
        link => requiresMarkdownDeliveryActions(link.qurlLink),
      );
      const embedArgs = {
        senderAlias,
        guildName,
        guildIconUrl,
        personalMessage: sendConfig.personal_message,
      };
      const allEmbeds = cappedLinks.map(link => {
        if (link.expiresAt <= Math.floor(Date.now() / 1000)) throw new Error('Minted link expired before delivery');
        return buildDeliveryEmbed({
          ...embedArgs,
          qurlLink: link.qurlLink,
          expiresAt: link.expiresAt,
          renderActionsAsMarkdown,
        });
      });
      const allComponents = renderActionsAsMarkdown
        ? []
        : packBulkDeliveryComponents(cappedLinks.map(link => link.qurlLink));

      result = await sendDM(recipient.id, { embeds: allEmbeds, components: allComponents });
    } catch (err) {
      logger.warn('Recipient delivery failed before completion', { sendId, errorName: err?.name ?? 'NonError' });
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
const RECIPIENT_REASON_ROLE_MEMBERS_UNAVAILABLE = 'Could not confirm all members of the picked roles. Select individual users instead';
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

// Slash-option choice arrays. The same wording flows into both the
// slash-command autocomplete and the confirm-card dropdowns so users
// see consistent labels at every step. EXPIRY_CHOICES already exists.
// SELF_DESTRUCT_CHOICES is new here because the confirm card's
// self-destruct StringSelect mirrors a slash option, so both surfaces
// share one source of truth.
const SELF_DESTRUCT_NO_TIMER_CHOICE = 'none';

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
// rendered as "N bot(s) filtered from this selection." A bot that
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
  const namedRoleMembersUnavailable = new Set();
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
  let iterationBoundHit = false;
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
        else namedRoleMembersUnavailable.add(roleId);
        continue;
      }
      if (isEveryoneRole && source.size === 0) {
        everyoneCacheCold = true;
        continue;
      }
      if (!isEveryoneRole) {
        // Role.members filters the guild cache; cached members can be incomplete.
        const cachedMembers = guild?.members?.cache?.size ?? 0;
        const knownIncomplete = Number.isSafeInteger(guild?.memberCount) && cachedMembers < guild.memberCount;
        if ((source.size === 0 && cachedMembers === 0) || knownIncomplete) {
          namedRoleMembersUnavailable.add(roleId);
          if (source.size === 0) continue;
        }
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
          if (!iterationBoundHit) {
            logger.debug('resolveMentionableSelection: ITER_BOUND hit during role expansion', {
              flow_id,
              role_id: roleId,
              inspected_from_roles: inspectedFromRoles,
              user_map_size: userMap.size,
              iter_bound: ITER_BOUND,
            });
            iterationBoundHit = true;
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
    unavailableRoleCount: namedRoleMembersUnavailable.size,
    iterationBoundHit,
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
 * unavailableRoleCount, roleMentionsDeniedNames). When adding or renaming a
 * signal, update BOTH surfaces in lockstep — the helpers don't share a copy table,
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
 *   iterationBoundHit?: boolean,
 *   massMentionDenied?: boolean,
 *   everyoneCacheCold?: boolean,
 *   unavailableRoleCount?: number,
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
  iterationBoundHit = false,
  massMentionDenied = false,
  everyoneCacheCold = false,
  unavailableRoleCount = 0,
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
    // user took a different picker action (selecting a role or @everyone rather
    // than an individual bot).
    lines.push(`• ${iterationBoundHit ? 'At least ' : ''}${droppedFromRoles} bot(s) filtered from this selection — skipped.`);
  }
  if (iterationBoundHit) {
    lines.push('• This selection could not be fully checked — pick individual users or smaller roles.');
  }
  if (everyoneCacheCold) {
    // Symmetric with droppedFromRoles: surfaced even on partial-valid
    // picks (named user + @everyone with cold cache → user lands but
    // @everyone silently expanded to zero, which the user would
    // otherwise have no way to know).
    lines.push('• Member cache not yet ready — `@everyone` expanded to 0 members. Try again in a few seconds.');
  }
  if (unavailableRoleCount > 0) lines.push(`• ${RECIPIENT_REASON_ROLE_MEMBERS_UNAVAILABLE}.`);
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
  if (
    mode === RECIPIENT_MODE_PICKER
    && interaction?.guild
    && interaction.memberPermissions?.has?.(PermissionFlagsBits.MentionEveryone) === true
  ) {
    const { count, accurate } = computeEveryoneDisplayCount(interaction.guild);
    if (accurate && count > config.QURL_SEND_MAX_RECIPIENTS) {
      content += `ℹ️ **@everyone is unavailable:** this server has ${count} human recipients, above the ${config.QURL_SEND_MAX_RECIPIENTS}-recipient limit. Pick individual recipients or a smaller role.\n`;
    }
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
  content += `**Requested expiry:** ${EXPIRY_LABELS[expiresIn] || (expiresIn === '7d' ? '7 days' : expiresIn)}\n`;
  if (isPositiveFinite(selfDestructSeconds)) {
    content += `**Self-destruct:** ${formatSelfDestructLabel(selfDestructSeconds)}\n`;
    const note = formatSelfDestructTimingNote(selfDestructSeconds);
    if (note) content += `${note}\n`;
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
  flow_id, sendNonce, sendDisabled, expiresIn, selfDestructSeconds, personalMessage,
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
    warnFlowCorruptionOnce({ flow_id, sendNonce, kind: 'self_destruct', value: selfDestructSeconds }, () => {
      logger.warn('renderConfirmCardRows: off-preset selfDestructSeconds in payload — falling back to No timer default', {
        selfDestructSeconds: truncForLog(selfDestructSeconds),
      });
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
  const legacyExpiry = expiresIn === '7d';
  if (!hasExpiryMatch && !legacyExpiry && expiresIn != null) {
    // Log so corrupted rows surface in forensics rather than just
    // getting papered over by the 24h fallback.
    warnFlowCorruptionOnce({ flow_id, sendNonce, kind: 'expiry', value: expiresIn }, () => {
      logger.warn('renderConfirmCardRows: off-EXPIRY_LABELS expiresIn in payload — falling back to 24h default', {
        expiresIn: truncForLog(expiresIn),
      });
    });
  }
  rows.push(new ActionRowBuilder().addComponents(
    new StringSelectMenuBuilder()
      .setCustomId(CONFIRM_EXPIRY_SELECT_CUSTOM_ID)
      .setPlaceholder(legacyExpiry ? 'Saved request: 7 days. Choose a new expiry to change it.' : 'Choose expiry')
      .setMinValues(1)
      .setMaxValues(1)
      .addOptions(
        ...EXPIRY_CHOICES.map((c) => ({
          label: c.name,
          value: c.value,
          default: hasExpiryMatch ? c.value === expiresIn : !legacyExpiry && c.value === '24h',
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
    // Keep the button label terse; content explains the warm-cache cap.
    // BOTTOM ROW COMPONENT BUDGET: Discord caps an ActionRow at 5
    // components. The current worst-case render is
    // [🔊 Voice] + [📢 @everyone] + Note + Send + Cancel = 5, exactly
    // at the limit. Adding another button below would silently throw
    // at discord.js builder time. Re-evaluate the layout (e.g., a
    // second row for affordances vs. fixed buttons) before adding.
    const everyoneDisabled = displayCount == null || displayCount === 0 || overCap;
    if (everyoneDisabled) {
      // Diagnostic: surface the exact state when the @everyone button
      // renders disabled. Reports like "why is @everyone disabled?"
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
      flags: MessageFlags.Ephemeral,
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
      await interaction.deferReply({ flags: MessageFlags.Ephemeral });
    }

    const recipientsRaw = interaction.options.getString('recipients');
    const expiresIn = interaction.options.getString('expires-in') || '24h';
    const selfDestructValue = interaction.options.getString('self-destruct') || SELF_DESTRUCT_NO_TIMER_CHOICE;
    const personalMessageRaw = interaction.options.getString('personal-message');

    // Defense-in-depth: expiresIn comes from the slash command's choice
    // list which Discord enforces server-side, but a forged interaction
    // could carry an off-set value. EXPIRY_LABELS owns the closed set.
    // Cached Discord command choices can still carry the retired 7d preset.
    if (!isValidSavedExpiry(expiresIn)) {
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
      await prewarmGuildMembersCache(interaction.guild, { caller: 'handleQurlSlashSend' }, pages => interaction.editReply({
        content: `Loading server members… ${pages * DISCORD_MEMBERS_PAGE_SIZE} checked.`,
      }));
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
      logger.debug('handleQurlSlashSend: all-invalid mention input', {
        resource_type: params.resourceType,
        dropped_bots: droppedBots,
        unresolved_ids: resolved.unresolvedIds.length,
        transient_failure_ids: resolved.transientFailureIds.length,
        invalid_tokens: parsed.invalidTokens.length,
        capped_count: parsed.cappedCount,
        mass_mention_denied: parsed.massMentionDenied,
        role_mentions_denied: parsed.roleMentionsDenied.length,
      });
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
    const storedRecipients = finalValid.map(u => ({ id: u.id, alias: resolveRecipientAlias(u, interaction) }));
    const payload = {
      resourceType: params.resourceType,
      attachment: params.attachment,
      locationUrl: params.locationUrl,
      locationName: params.locationName,
      resourceLabel: params.resourceLabel,
      // Keep new flows legacy until the reader fleet rollout is complete.
      ...withFlowRecipients({}, storedRecipients),
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
      flow_id, sendNonce,
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
    const reply = await interaction.editReply({ content, components: rows });
    logger.audit(AUDIT_EVENTS.SEND_CONFIRM_OPENED, {
      flow_id, user_id: interaction.user.id, send_nonce: sendNonce, resource_type: params.resourceType,
      recipient_count: recipientIds.length, needs_picker: needsPicker, expires_in: expiresIn,
      has_self_destruct: selfDestructSeconds !== null, has_personal_message: Boolean(personalMessage),
    });
    return reply;
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
      .catch(() => interaction.reply({ content: errContent, flags: MessageFlags.Ephemeral }).catch(logIgnoredDiscordErr));
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
      flags: MessageFlags.Ephemeral,
    });
  }

  // UX fast-fail BEFORE setCooldown — "bot too busy" is server-side
  // backpressure, not user fault; locking the user out for 30s on a
  // capacity rejection would be punitive.
  if (activeFileSends >= MAX_CONCURRENT_FILE_SENDS) {
    logger.audit(AUDIT_EVENTS.UPLOAD_REJECTED_CAPACITY, {
      active_file_sends: activeFileSends, max_concurrent: MAX_CONCURRENT_FILE_SENDS,
    });
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
      flags: MessageFlags.Ephemeral,
    });
  }

  // Cooldown gate at entry — after capacity backpressure but BEFORE
  // any input-validation so invalid inputs (malformed attachment,
  // SSRF host, wrong type, oversize) still throttle. Matches the
  // pattern at commands.js:~1677.
  if (isOnCooldown(interaction.user.id)) {
    return interaction.reply({
      content: 'Please wait before sending again.',
      flags: MessageFlags.Ephemeral,
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
      flags: MessageFlags.Ephemeral,
    });
  }
  if (!attachment || typeof attachment.url !== 'string') {
    // Same client/schema-desync rationale — clear cooldown for retry.
    clearCooldown(interaction.user.id);
    return interaction.reply({
      content: '❌ Attachment is missing or malformed.',
      flags: MessageFlags.Ephemeral,
    });
  }
  if (!isAllowedSourceUrl(attachment.url)) {
    logger.warn('handleQurlSend: attachment.url failed SSRF gate', {
      user_id: interaction.user.id, host: safeUrlHost(attachment.url),
    });
    return interaction.reply({
      content: '❌ Attachment source not allowed. Files must be uploaded via Discord, not linked from external URLs.',
      flags: MessageFlags.Ephemeral,
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
      flags: MessageFlags.Ephemeral,
    });
  }
  if (attachment.size > MAX_FILE_SIZE) {
    // Same shape as the file-type rejection — honest user error.
    clearCooldown(interaction.user.id);
    return interaction.reply({
      content: `❌ File too large (${Math.round(attachment.size / 1024 / 1024)}MB). Maximum is ${Math.round(MAX_FILE_SIZE / 1024 / 1024)}MB.`,
      flags: MessageFlags.Ephemeral,
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
      flags: MessageFlags.Ephemeral,
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
      flags: MessageFlags.Ephemeral,
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
      flags: MessageFlags.Ephemeral,
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
      flags: MessageFlags.Ephemeral,
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
      flags: MessageFlags.Ephemeral,
    });
  }

  // Defer before Places resolve to stay inside Discord's 3 s ACK window.
  // handleQurlSlashSend below checks `interaction.deferred` and skips
  // its own defer.
  try {
    await interaction.deferReply({ flags: MessageFlags.Ephemeral });
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
  if (!await deferUpdateOrStop(interaction, flow_id)) return undefined;

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
    unavailableRoleCount,
    iterationBoundHit,
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
    && !iterationBoundHit
    && !everyoneCacheCold
    && unavailableRoleCount === 0
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
      flow_id, sendNonce: payload.sendNonce,
      sendDisabled: true,
      expiresIn: payload.expiresIn,
      selfDestructSeconds: payload.selfDestructSeconds,
      personalMessage: payload.personalMessage,
      voiceChannelId: payload.voiceChannelId,
      interaction,
      // Mirror payload.recipientIds (unchanged on the reject branch — no
      // transitionFlow fires here) so the user can recover their prior
      // valid selection on re-open instead of starting from empty.
      recipientIds: readFlowRecipients(payload).map(recipient => recipient.id),
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
      iteration_bound_hit: iterationBoundHit,
      everyone_cache_cold: everyoneCacheCold,
      named_role_members_unavailable_count: unavailableRoleCount,
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
    // unavailable roles → massMentionDenied → roleMentionsDenied) so the two surfaces
    // present multi-signal picks in the same sequence. Most picks
    // hit at most one signal so ordering is rarely user-visible,
    // but pin the parity here against future drift.
    const reasons = [];
    if (droppedBots > 0) reasons.push(RECIPIENT_REASON_BOTS_DROPPED);
    if (iterationBoundHit) reasons.push('This selection could not be fully checked — pick individual users or smaller roles');
    else if (droppedFromRoles > 0) reasons.push('Bot accounts from this selection were excluded');
    if (everyoneCacheCold) reasons.push('Member cache not yet ready — try again in a few seconds');
    if (unavailableRoleCount > 0) reasons.push(RECIPIENT_REASON_ROLE_MEMBERS_UNAVAILABLE);
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
    iterationBoundHit,
    massMentionDenied,
    everyoneCacheCold,
    unavailableRoleCount,
    roleMentionsDeniedNames,
  });
  const newRecipients = valid.map(u => ({ id: u.id, alias: resolveRecipientAlias(u, interaction) }));
  // Asymmetry vs. the menu/modal handlers' no-op short-circuits:
  // the picker DOES NOT skip transitionFlow on a same-recipient-set
  // re-pick. Re-running here recomputes warningsBlock (droppedBots
  // can flip if the picker selection now includes a bot) plus
  // selfIncluded (flips if sender enters or leaves the pick), and
  // refreshes recipientAliases (display names may have changed
  // since the prior pick). All three are user-visible and worth
  // the version bump.
  const newPayload = {
    ...withFlowRecipients(payload, newRecipients),
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
      ...(row.payloadSnapshot !== undefined && { expectedPayload: row.payloadSnapshot }),
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
      flags: MessageFlags.Ephemeral,
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

  logger.audit(AUDIT_EVENTS.SEND_CONFIRM_PICKED, {
    flow_id, user_id: interaction.user.id, recipient_count: valid.length,
  });

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
      flow_id, sendNonce: payload.sendNonce,
      sendDisabled: false,
      expiresIn: payload.expiresIn,
      selfDestructSeconds: payload.selfDestructSeconds,
      personalMessage: payload.personalMessage,
      voiceChannelId: payload.voiceChannelId,
      interaction,
      recipientIds: readFlowRecipients(newPayload).map(recipient => recipient.id),
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
  if (!await deferUpdateOrStop(interaction, flow_id)) return undefined;

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
    return rerenderConfirmCard(interaction, flow_id, {
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
      flow_id, sendNonce: payload.sendNonce,
      sendDisabled: true,
      expiresIn: payload.expiresIn,
      selfDestructSeconds: payload.selfDestructSeconds,
      personalMessage: payload.personalMessage,
      voiceChannelId,
      interaction,
      // Mirror payload.recipientIds (unchanged on the reject branch —
      // no transitionFlow fires) so the user can recover their prior
      // valid selection on re-open of the picker.
      recipientIds: readFlowRecipients(payload).map(recipient => recipient.id),
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
  const newRecipients = valid.map(u => ({ id: u.id, alias: resolveRecipientAlias(u, interaction) }));
  const newPayload = {
    ...withFlowRecipients(payload, newRecipients),
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
      ...(row.payloadSnapshot !== undefined && { expectedPayload: row.payloadSnapshot }),
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
      flags: MessageFlags.Ephemeral,
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
      flow_id, sendNonce: payload.sendNonce,
      sendDisabled: false,
      expiresIn: payload.expiresIn,
      selfDestructSeconds: payload.selfDestructSeconds,
      personalMessage: payload.personalMessage,
      voiceChannelId,
      interaction,
      recipientIds: readFlowRecipients(newPayload).map(recipient => recipient.id),
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
  if (!await deferUpdateOrStop(interaction, flow_id)) return undefined;

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
    ...withFlowRecipients(payload, []),
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
      ...(row.payloadSnapshot !== undefined && { expectedPayload: row.payloadSnapshot }),
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
      flags: MessageFlags.Ephemeral,
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
  return rerenderConfirmCard(interaction, flow_id, newPayload);
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
  if (!await deferUpdateOrStop(interaction, flow_id)) return undefined;

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
      flow_id, sendNonce: payload.sendNonce,
      sendDisabled: true,
      expiresIn: payload.expiresIn,
      selfDestructSeconds: payload.selfDestructSeconds,
      personalMessage: payload.personalMessage,
      voiceChannelId: payload.voiceChannelId,
      interaction,
      recipientIds: readFlowRecipients(payload).map(recipient => recipient.id),
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
  const newRecipients = valid.map(u => ({ id: u.id, alias: resolveRecipientAlias(u, interaction) }));
  // TODO(upstream-contract): Discord caps MentionableSelect default_values at 25,
  // so @everyone replaces any truncated picker selection.
  const newPayload = {
    ...withFlowRecipients(payload, newRecipients),
    warningsBlock: newWarningsBlock,
    selfIncluded,
    recipientMode: RECIPIENT_MODE_EVERYONE,
  };
  let result;
  try {
    result = await transitionFlow(flow_id, row.version, {
      ...(row.payloadSnapshot !== undefined && { expectedPayload: row.payloadSnapshot }),
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
      flags: MessageFlags.Ephemeral,
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

  return rerenderConfirmCard(interaction, flow_id, newPayload);
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
async function rerenderConfirmCard(interaction, flow_id, newPayload) {
  const storedRecipients = readFlowRecipients(newPayload);
  const recipientIds = storedRecipients.map(recipient => recipient.id);
  // Resolve each id through three layers, in priority order:
  //   1. members.cache — freshest data when populated
  //   2. stored recipient alias — persisted at pick time, survives
  //      cache eviction between pick and menu-click
  //   3. fabricated `user-${id}` fallback — last resort if both miss
  // resolveRecipientAlias does its own NFKC + bidi/zero-width strip
  // on whichever wins, so the rendered text is always sanitized.
  // The persisted aliases were ALREADY produced by
  // resolveRecipientAlias at pick time — re-running it here is a
  // no-op because NFKC + bidi/zero-width strip have fixed points
  // after one pass. If sanitize semantics ever stop being idempotent,
  // this becomes a double-sanitize bug.
  const memberCache = interaction.guild?.members?.cache;
  const validRecipients = storedRecipients.map(({ id, alias }) => {
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
      flow_id, sendNonce: newPayload.sendNonce,
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

async function refreshConflictedConfirmCard(interaction, flow_id, row) {
  let fresh;
  try {
    fresh = await loadFlow(flow_id);
  } catch (err) {
    logger.warn('confirm-card conflict refresh failed', { flow_id, errorName: err?.name });
    return interaction.followUp({
      content: 'Could not refresh the card. Please try again.',
      flags: MessageFlags.Ephemeral,
    }).catch(logIgnoredDiscordErr);
  }
  // A reused flow_id can belong to a newer send; only refresh this card.
  const nonce = row.payload?.sendNonce;
  if (fresh?.stage === SEND_STAGE_AWAITING_CONFIRM && typeof nonce === 'string'
    && /^[a-f0-9]{16}$/.test(nonce) && fresh.payload?.sendNonce === nonce) {
    await rerenderConfirmCard(interaction, flow_id, fresh.payload);
    return interaction.followUp({
      content: 'Another change landed first. Try your change again if you still want it.',
      flags: MessageFlags.Ephemeral,
    }).catch(logIgnoredDiscordErr);
  }
  return interaction.editReply({
    content: 'Send was superseded — re-run the command.',
    components: [],
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
  // Chunked routes may already be acknowledged by the dispatcher.
  if (!picked || !isValidExpiry(picked)) {
    logger.warn('handleConfirmExpirySelect: forged off-set expiry value', {
      flow_id, value: truncForLog(picked),
    });
    return interaction[interaction.deferred || interaction.replied ? 'followUp' : 'reply']({
      content: '❌ Unrecognized expiry value. Re-pick from the list.',
      flags: MessageFlags.Ephemeral,
    }).catch(logIgnoredDiscordErr);
  }
  if (!await deferUpdateOrStop(interaction, flow_id)) return undefined;
  const payload = row.payload || {};
  // No-op re-pick (same value as current state) → skip the DDB write
  // + version bump. A version bump would needlessly fence any
  // concurrent sibling interaction (picker / self-destruct / note
  // modal) that's mid-flight. Still re-render the card so the user
  // gets visible feedback that their click registered.
  if (picked === payload.expiresIn) {
    return rerenderConfirmCard(interaction, flow_id, payload);
  }
  const newPayload = { ...payload, expiresIn: picked };
  let result;
  try {
    result = await transitionFlow(flow_id, row.version, {
      ...(row.payloadSnapshot !== undefined && { expectedPayload: row.payloadSnapshot }),
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
      flags: MessageFlags.Ephemeral,
    }).catch(logIgnoredDiscordErr);
  }
  if (result.result === 'conflict') {
    return refreshConflictedConfirmCard(interaction, flow_id, row);
  }
  if (result.result === 'not_found') {
    return interaction.editReply({
      content: 'This send expired — re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }
  return rerenderConfirmCard(interaction, flow_id, newPayload);
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
  // Validate against the closed legitimate set before any flow mutation.
  // Discord enforces the choice list server-side; an off-set value
  // here means a forged interaction. Silently mapping forged values
  // to null would clear a user's previously-set timer on every probe
  // — symmetric with the expiry handler's reject-and-warn behavior.
  if (!isLegitimateSelfDestructSelectValue(pickedValue)) {
    logger.warn('handleConfirmSelfDestructSelect: forged off-set self-destruct value', {
      flow_id, value: truncForLog(pickedValue),
    });
    return interaction[interaction.deferred || interaction.replied ? 'followUp' : 'reply']({
      content: '❌ Unrecognized self-destruct value. Re-pick from the list.',
      flags: MessageFlags.Ephemeral,
    }).catch(logIgnoredDiscordErr);
  }
  if (!await deferUpdateOrStop(interaction, flow_id)) return undefined;
  const selfDestructSeconds = selfDestructSelectValueToSeconds(pickedValue);
  const payload = row.payload || {};
  // No-op re-pick (same value as current state) → skip the write +
  // version bump (see expiry handler for the full rationale).
  if (selfDestructSeconds === payload.selfDestructSeconds) {
    return rerenderConfirmCard(interaction, flow_id, payload);
  }
  const newPayload = { ...payload, selfDestructSeconds };
  let result;
  try {
    result = await transitionFlow(flow_id, row.version, {
      ...(row.payloadSnapshot !== undefined && { expectedPayload: row.payloadSnapshot }),
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
      flags: MessageFlags.Ephemeral,
    }).catch(logIgnoredDiscordErr);
  }
  if (result.result === 'conflict') {
    return refreshConflictedConfirmCard(interaction, flow_id, row);
  }
  if (result.result === 'not_found') {
    return interaction.editReply({
      content: 'This send expired — re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }
  return rerenderConfirmCard(interaction, flow_id, newPayload);
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
      flags: MessageFlags.Ephemeral,
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
  if (!await deferUpdateOrStop(interaction, flow_id)) return undefined;
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
      flags: MessageFlags.Ephemeral,
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
    return rerenderConfirmCard(interaction, flow_id, payload);
  }
  const newPayload = { ...payload, personalMessage, personalMessageRaw };
  let result;
  try {
    result = await transitionFlow(flow_id, row.version, {
      ...(row.payloadSnapshot !== undefined && { expectedPayload: row.payloadSnapshot }),
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
      flags: MessageFlags.Ephemeral,
    }).catch(logIgnoredDiscordErr);
  }
  if (result.result === 'conflict') {
    return refreshConflictedConfirmCard(interaction, flow_id, row);
  }
  if (result.result === 'not_found') {
    return interaction.editReply({
      content: 'This send expired — re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }
  return rerenderConfirmCard(interaction, flow_id, newPayload);
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
  // alone can chew through the budget. If Discord already accepted
  // another copy's acknowledgement, this copy does not own the action;
  // deferUpdateOrStop prevents any state change or send.
  //
  // All ephemeral error-replies below switch from `interaction.reply`
  // to `interaction.followUp` (the interaction is now in the
  // deferred state and `.reply` would throw); main-message updates
  // switch from `interaction.update` to `interaction.editReply`.
  if (!await deferUpdateOrStop(interaction, flow_id)) return undefined;

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
      content: '❌ qURL is no longer in this server. Re-invite it and re-run the command.',
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
  const recipientIds = readFlowRecipients(payload).map(recipient => recipient.id);
  if (recipientIds.length === 0) {
    // Version-gate the delete: a concurrent UserSelect could have
    // transitioned the row between the dispatcher's loadFlow (which
    // fed our stale `payload` view) and this click. If `deleted:
    // false`, the more-recent row is preserved and the user sees the
    // "card moved" recovery copy instead of a silently wiped pick.
    const deleteResult = await deleteFlow(flow_id, {
      stage: SEND_STAGE_AWAITING_CONFIRM,
      reason: 'terminal',
      expectedVersion: row.version,
      ...(row.payloadSnapshot !== undefined && { expectedPayload: row.payloadSnapshot }),
    }).catch((err) => {
      logger.warn('handleConfirmSendClick: deleteFlow on empty-recipients failed', {
        flow_id, error: err && err.message,
      });
      return { deleted: false };
    });
    if (!deleteResult.deleted) {
      return interaction.followUp({
        content: 'The card moved — re-check it and click Send again.',
        flags: MessageFlags.Ephemeral,
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
    resolveRecipientUsers(interaction, recipientIds),
    db.getGuildApiKey(interaction.guildId),
  ]);
  if (apiKeyResult.status === 'rejected' && apiKeyResult.reason?.code === 'GUILD_SETUP_REQUIRED') {
    clearCooldown(interaction.user.id);
    return interaction.followUp({
      content: GUILD_SETUP_REQUIRED_REPLY, flags: MessageFlags.Ephemeral,
    }).catch(logIgnoredDiscordErr);
  }
  if (resolveResult.status === 'rejected') {
    logger.error('handleConfirmSendClick: resolveRecipientUsers threw', {
      flow_id, error: resolveResult.reason && resolveResult.reason.message,
    });
    // Zero side effects — unlock retry so the user can re-click Send
    // immediately after the transient blip clears.
    clearCooldown(interaction.user.id);
    return interaction.followUp({
      content: '❌ Could not look up recipients right now. Try **Send** again in a moment.',
      flags: MessageFlags.Ephemeral,
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
      ...(row.payloadSnapshot !== undefined && { expectedPayload: row.payloadSnapshot }),
    }).catch((err) => {
      logger.warn('handleConfirmSendClick: deleteFlow on empty failed', {
        flow_id, error: err && err.message,
      });
      return { deleted: false };
    });
    if (!deleteResult.deleted) {
      return interaction.followUp({
        content: 'The card moved — re-check it and click Send again.',
        flags: MessageFlags.Ephemeral,
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
  // within the TTL after the admin reruns `/qurl-admin setup`. Burning the
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
      flags: MessageFlags.Ephemeral,
    }).catch(logIgnoredDiscordErr);
  }
  const guildApiKey = apiKeyResult.value;
  // Check the resolved key BEFORE deleteFlow. If both guildApiKey and
  // config.QURL_API_KEY are null (rare: key rotation removed the key
  // between dispatcher pre-check and Send click), there's nothing to
  // send with — preserving the flow row lets the user retry after the
  // admin re-runs `/qurl-admin setup` without re-invoking the slash command.
  // Cooldown clears so the user isn't stranded for the 30s window
  // while waiting on the admin.
  const apiKey = guildApiKey || config.QURL_API_KEY;
  if (!apiKey) {
    clearCooldown(interaction.user.id);
    return interaction.editReply({
      content: '❌ qURL is no longer configured for this server. Ask an admin to run `/qurl-admin setup`.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }

  const deleteResult = await deleteFlow(flow_id, {
    stage: SEND_STAGE_AWAITING_CONFIRM,
    reason: 'terminal',
    expectedVersion: row.version,
    ...(row.payloadSnapshot !== undefined && { expectedPayload: row.payloadSnapshot }),
  });
  if (!deleteResult.deleted) {
    return interaction.followUp({
      content: 'Recipients changed before Send fired — re-check the card and click Send again.',
      flags: MessageFlags.Ephemeral,
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
      flags: MessageFlags.Ephemeral,
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
  if (!await deferUpdateOrStop(interaction, flow_id)) return undefined;
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
      ...(row.payloadSnapshot !== undefined && { expectedPayload: row.payloadSnapshot }),
    }));
  } catch (err) {
    logger.error('handleConfirmCancelClick: deleteFlow threw', {
      flow_id, error: err && err.message,
    });
    return interaction.followUp({
      content: '❌ Could not cancel right now — try clicking **Cancel** again in a moment.',
      flags: MessageFlags.Ephemeral,
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
      flags: MessageFlags.Ephemeral,
    }).catch(logIgnoredDiscordErr);
  }
  // Picker transitions renew expiry; created_at keeps the original dwell time.
  const now = Math.floor(Date.now() / 1000);
  const dwell = Number.isSafeInteger(row.created_at) && row.created_at >= 0 && row.created_at <= now
    ? { dwell_seconds: now - row.created_at } : {};
  logger.audit(AUDIT_EVENTS.SEND_CONFIRM_CANCELLED, {
    flow_id, user_id: interaction.user.id, ...dwell,
  });
  clearCooldown(interaction.user.id);
  return interaction.editReply({
    content: 'Send cancelled.',
    components: [],
  }).catch(logIgnoredDiscordErr);
}

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
function renderRevokeMsg(sendId, names, total, showAll, success, finalizationFailed = false) {
  let data;
  try {
    data = renderRevokeContent({ names, total, showAll, success, finalizationFailed });
  } catch (err) {
    data = {
      content: revokeRenderFallback(sendId, success, total, err),
      needsExpand: false,
      attachmentText: null,
    };
  }
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

function revokeRenderFallback(sendId, success, total, err) {
  logger.error('Failed to render revoke result', {
    sendId,
    success,
    total,
    error: err?.message ?? String(err),
  });
  // DELETEs may already have completed. Avoid a dead Discord interaction
  // while making no claim about an outcome whose counts are inconsistent.
  return 'qURL could not display the revocation result. If this send still appears in `/qurl revoke`, retry it there.';
}

// Builds the post-send confirmation body. When the full inline render
// would exceed Discord's 2000-char content cap, falls back to a
// `recipients.txt` attachment; "(see attached)" is appended only to
// lines that were actually truncated.
function renderSendConfirm({
  delivered, expiresIn, selfDestructSeconds,
  failedNamesPlain = [], successNames = [], showAll = false,
}) {
  const header = `Sent to ${delivered} user${delivered !== 1 ? 's' : ''} | Requested expiry: ${expiresIn} | ${formatSelfDestructSegment(selfDestructSeconds)}`;
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
  // Stop destructive work before the 13-minute interaction result deadline.
  const workDeadline = Date.now() + REVOKE_WORK_DEADLINE_MS;
  const signal = AbortSignal.timeout(REVOKE_WORK_DEADLINE_MS);
  // Establish the durable no-more-recipients barrier before reading the send
  // rows. getSendItems uses a strongly consistent base-table query, so an Add
  // transaction that committed before this barrier is included; one racing
  // after it fails the revoking_at condition check.
  const barrierEstablished = await db.markSendRevoking(sendId, senderDiscordId, { signal });
  // The durable store returns false for unknown, finalized, or foreign sends.
  // Never issue a DELETE unless it positively confirms the barrier.
  if (!barrierEstablished) {
    return {
      barrierEstablished: false,
      finalizationFailed: false,
      success: 0,
      total: 0,
      successUserIds: [],
      failureUserIds: [],
    };
  }

  // Items carry dm_channel_id / dm_message_id / dm_status so the post-
  // revoke step can edit each strict-success recipient's DM in place.
  // Legacy rows predating that wire-up have the refs unset — the edit
  // step skips them.
  const items = await db.getSendItems(sendId, senderDiscordId, { consistentRead: true, signal });

  // Revoke each resource's children together, retaining per-child confirmations
  // even if a sibling fails. The connector chunks child-revoke requests.
  // A row without a usable qurl_id cannot be revoked: a watermarked child lives
  // on the connector's tunnel, so deleting the shared parent would neither
  // reach it nor be safe for other sends. Only that row's recipient fails;
  // identifiable siblings on the same resource still revoke and succeed.
  const byResource = new Map();
  const invalidResourceRecipientIds = new Set();
  for (const item of items) {
    const qurlId = qurlIdForCleanup(item.qurl_id);
    if (typeof item.resource_id !== 'string' || item.resource_id.trim().length === 0 || qurlId === null) {
      invalidResourceRecipientIds.add(item.recipient_discord_id);
      continue;
    }
    const list = byResource.get(item.resource_id) || [];
    list.push({ recipientId: item.recipient_discord_id, qurlId });
    byResource.set(item.resource_id, list);
  }
  const resourceEntries = [...byResource.entries()];
  const totalUsers = new Set(items.map(it => it.recipient_discord_id)).size;

  const successUserIds = [];
  const failureUserIds = [];

  const results = await batchSettled(resourceEntries, async ([resourceId, children]) => {
    signal.throwIfAborted();
    await revokeMintedLinks(resourceId, children.map(child => child.qurlId), apiKey, { signal });
    return resourceId;
  }, 5);

  // User-centric: strict-success = recipient whose every link was
  // revoked (in success but not in failure). Mixed-outcome users
  // (some links succeeded, some failed) count as failure — better to
  // tell the operator "alice is partial" via failure than misleadingly
  // claim full success.
  const seenSuccess = new Set();
  const seenFailure = new Set(invalidResourceRecipientIds);
  if (invalidResourceRecipientIds.size > 0) {
    logger.error('Cannot revoke send row with missing resource or token identity', {
      sendId,
      affectedRecipients: invalidResourceRecipientIds.size,
    });
  }
  for (let i = 0; i < results.length; i++) {
    const [resourceId, children] = resourceEntries[i];
    const recipientIds = children.map(child => child.recipientId);
    if (results[i].status === 'fulfilled') {
      for (const id of recipientIds) seenSuccess.add(id);
    } else {
      const confirmed = new Set(Array.isArray(results[i].reason?.confirmedQurlIds)
        ? results[i].reason.confirmedQurlIds : []);
      for (const child of children) {
        if (confirmed.has(child.qurlId)) seenSuccess.add(child.recipientId);
        else seenFailure.add(child.recipientId);
      }
      logger.error('Failed to revoke qURL', {
        resource_ref: resourceIdLogRef(resourceId),
        error: results[i].reason?.message,
        failed_child_count: results[i].reason?.failedCount ?? null,
        fallback_error: results[i].reason?.fallbackError?.message ?? null,
      });
    }
  }
  // Strict success = revoked AND not in any failure bucket.
  for (const id of seenSuccess) {
    if (!seenFailure.has(id)) successUserIds.push(id);
  }
  for (const id of seenFailure) failureUserIds.push(id);

  const success = successUserIds.length;
  const total = totalUsers;
  // Audit success/total describe actual per-resource revoke confirmations.
  // Malformed rows never fabricate a revoke denominator; report their
  // affected recipients in a separate field while keeping finalization
  // fail-closed.
  const auditTotal = byResource.size;
  const auditSuccess = results.filter(r => r.status === 'fulfilled').length;
  const unresolvableRecipients = invalidResourceRecipientIds.size;
  const fullyConfirmed = auditSuccess === auditTotal && unresolvableRecipients === 0;

  // Emit audit after DELETE attempts so the tally reflects actual qURL API
  // outcomes. The revocation-intent write happened above, before any
  // destructive side effect.
  if (total > 0) {
    const event = fullyConfirmed ? AUDIT_EVENTS.REVOKE_SUCCESS : AUDIT_EVENTS.REVOKE_FAILED;
    logger.audit(event, {
      send_id: sendId,
      success: auditSuccess,
      total: auditTotal,
      unresolvable_recipients: unresolvableRecipients,
    });
  }

  // Top-level `success/total` are per-resource revoke confirmations; the
  // nested `users` tally is the operator-facing per-recipient result.
  logger.info('Revoked send', {
    sendId,
    success: auditSuccess,
    total: auditTotal,
    unresolvable_recipients: unresolvableRecipients,
    users: { success, total },
  });

  // TODO(upstream-contract): qurl-service's
  // TestRevokeQurl_Idempotent_SecondRevokeDoesNotRepublish pins repeated
  // DELETE /v1/qurls/{id} as a 204 no-op, so confirmed partial work is retryable.
  // 404 can mean wrong owner; 401/403 can recover after setup. None confirms
  // deletion. Keep the barrier until every child and finalization are confirmed.
  // Finalize confirmed deletes before best-effort DM edits can consume the
  // remaining deadline. Failed/uncertain writes keep the durable retry barrier.
  let finalizationFailed = false;
  if (fullyConfirmed) {
    try {
      const finalized = await db.markSendRevoked(sendId, senderDiscordId, { signal });
      if (finalized !== true) throw new Error('finalization was not confirmed');
    } catch (err) {
      finalizationFailed = true;
      if (signal.aborted) {
        logger.info('Revoke finalization reached work deadline', { sendId });
      } else {
        logger.error('Failed to finalize revoked send state', {
          sendId,
          error: err?.message ?? String(err),
        });
      }
    }
  }

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
  // A retry deliberately re-PATCHes recipients whose DELETE succeeded on a
  // prior attempt. PATCH replaces the message with the same terminal payload,
  // so the operation is idempotent; persisting a second per-DM marker would
  // introduce a write/edit race that could permanently suppress the rewrite.
  if (success > 0 && !signal.aborted) {
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
      const editWork = batchSettled(entries, async ([, refs]) => {
        const res = await editDM(refs.channelId, refs.messageId, editPayload, { signal });
        if (!res.ok) {
          const e = new Error('editDM returned not-ok');
          e.expected = res.expected;
          throw e;
        }
        return res;
      }, 5);
      if (await settlesWithin(editWork, Math.max(0, workDeadline - Date.now()))) {
        const editResults = await editWork;
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
      } else {
        logger.info('Revoke DM updates reached work deadline', { sendId });
      }
    }
  }

  // failureUserIds is computed but not yet rendered by name. The shared
  // header reports the exact unconfirmed count and tells the operator to
  // retry/reconnect; callers can use this list for a future named-failure
  // detail without inferring that DELETE failure means the link was opened.
  return {
    barrierEstablished: true,
    finalizationFailed,
    success,
    total,
    successUserIds,
    failureUserIds,
  };
}

module.exports = {
  SELF_DESTRUCT_NO_TIMER_CHOICE,
  CONFIRM_USER_SELECT_CUSTOM_ID,
  SEND_STAGE_AWAITING_CONFIRM,
  handleConfirmUserSelect,
  CONFIRM_SEND_CUSTOM_ID,
  handleConfirmSendClick,
  CONFIRM_CANCEL_CUSTOM_ID,
  handleConfirmCancelClick,
  CONFIRM_EXPIRY_SELECT_CUSTOM_ID,
  handleConfirmExpirySelect,
  CONFIRM_SELF_DESTRUCT_SELECT_CUSTOM_ID,
  handleConfirmSelfDestructSelect,
  CONFIRM_NOTE_BUTTON_CUSTOM_ID,
  handleConfirmNoteButton,
  CONFIRM_NOTE_MODAL_CUSTOM_ID,
  handleConfirmNoteModal,
  CONFIRM_VOICE_EVERYONE_BUTTON_CUSTOM_ID,
  handleConfirmVoiceEveryone,
  CONFIRM_PICK_MANUAL_BUTTON_CUSTOM_ID,
  handleConfirmPickManual,
  CONFIRM_EVERYONE_BUTTON_CUSTOM_ID,
  handleConfirmEveryone,
  clearCooldown,
  revokingSendLocks,
  sendCooldowns,
  resolveSenderAlias,
  safeUrlHost,
  revokeAllLinks,
  handleQurlSend,
  handleQurlMap,
  evictOldest,
  COOLDOWNS_MAX,
  logIgnoredDiscordErr,
  ALREADY_REVOKING_SEND_MSG,
  REVOKE_SELECT_RESULT_WAIT_MS,
  REVOKE_RUNNING_MSG,
  revokeRenderFallback,
  EXPIRY_CHOICES,
  PERSONAL_MESSAGE_INPUT_MAX,
  capUtf16Units,
  ...(process.env.NODE_ENV !== 'production' && {
    logSafeApiDetail,
    isGoogleMapsURL,
    sanitizeMessage,
    isAllowedFileType,
    isOnCooldown,
    setCooldown,
    batchSettled,
    renderConfirmCardRows,
    everyoneCountMemo,
    computeEveryoneDisplayCount,
    handleAddRecipients,
    buildDeliveryPayload,
    buildDeliveryEmbed,
    buildStepThroughButton,
    buildTrustButton,
    packBulkDeliveryComponents,
    buildRevokedDMPayload,
    persistDispatchResult,
    resolveGuildProvenance,
    monitorLinkStatus,
    renderRevokeMsg,
    renderSendConfirm,
    mintLinksInBatches,
    cleanupFreshMintedLinks,
    activeMonitors,
    executeSendPipeline,
    largeSendThreshold,
    LARGE_SEND_RECIPIENT_FLOOR,
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
    safeDecodeURIComponent,
    RECIPIENT_MODE_PICKER,
    RECIPIENT_MODE_VOICE,
    RECIPIENT_MODE_EVERYONE,
    normalizeRecipientMode,
    SEND_FLOW_TTL_SECONDS,
    classifyMintFailure,
    getActiveFileSends: () => activeFileSends,
    setActiveFileSends: (n) => { activeFileSends = n; },
  }),
};
