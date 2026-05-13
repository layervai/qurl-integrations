const {
  SlashCommandBuilder,
  EmbedBuilder,
  PermissionFlagsBits,
  ActionRowBuilder,
  ButtonBuilder,
  ButtonStyle,
  ChannelType,
  ComponentType,
  StringSelectMenuBuilder,
  UserSelectMenuBuilder,
  ModalBuilder,
  TextInputBuilder,
  TextInputStyle,
  AttachmentBuilder,
} = require('discord.js');
const crypto = require('crypto');
const config = require('./config');
const db = require('./store');
const logger = require('./logger');
const { COLORS, TIMEOUTS, RESOURCE_TYPES, DM_STATUS, MAX_FILE_SIZE, MAX_CONCURRENT_MONITORS, AUDIT_EVENTS } = require('./constants');
const {
  expiryToISO,
  expiryToMs,
  formatSelfDestructLabel,
  selfDestructSelectValueToSeconds,
  SELF_DESTRUCT_PRESETS,
  SELF_DESTRUCT_NO_TIMER_VALUE,
} = require('./utils/time');
const { requireAdmin } = require('./utils/admin');
const { signQurlOAuthState } = require('./utils/qurl-oauth-state');
const { deleteLink, getResourceStatus } = require('./qurl');
const { downloadAndUpload, reUploadBuffer, mintLinks, uploadJsonToConnector, isAllowedSourceUrl } = require('./connector');
const { deleteFlow, transitionFlow, supersedeOrCreate } = require('./flow-state');
const { flowIdForInteraction, registerFlow, safeReply, siblingMessageForStage } = require('./flow-dispatch');

// Max tokens the QURL API allows per resource. When exceeded, a new
// resource must be created (re-upload) to get a fresh token pool.
const TOKENS_PER_RESOURCE = 10;

// Shared helper: many Discord API calls (edits, updates, follow-ups) are
// best-effort — if the interaction token expired or Discord is briefly
// degraded, we log a warning and continue rather than fail the whole flow.
// Extracted to deduplicate ~13 identical `.catch(err => logger.warn(...))`
// one-liners across this file.
const logIgnoredDiscordErr = (err) => logger.warn('Discord API op failed (ignored)', { error: err.message });
const { getChannelMembers, sendDM } = require('./discord');


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



const { sanitizeFilename, escapeDiscordMarkdown, sanitizeDisplayName, sanitizeDisplayNamePlain } = require('./utils/sanitize');

// Best-effort host extraction for log lines. URL parsing throws on
// pathological input (no scheme, embedded null, etc.) — swallow and
// return a marker so a log line is still useful in triage.
function safeUrlHost(url) {
  try { return new URL(url).host; } catch { return 'invalid-url'; }
}

function sanitizeMessage(msg) {
  // Order matters: strip @-mention abuse first (the closing `>` of `<@123>`
  // would otherwise be escaped and the mention regex wouldn't match). Then
  // escape Discord markdown so a crafted message like
  // `[Free Prizes](https://phishing.com)` can't render as a masked link.
  // The `[mention]` literal we insert below is re-applied post-escape as a
  // plain substitution so the brackets stay visible to the user.
  // Sentinel survives the markdown-escape pass unchanged: it contains no
  // chars in the escape regex ([\*~`>|\[\]()\\_]). Suffix/prefix of random
  // hex so it can't collide with anything a user would plausibly type.
  const MENTION_SENTINEL = 'XMENTIONX74caf3b0e79aXMENTIONX';
  const stripped = msg
    .replace(/@(everyone|here)/gi, '@\u200b$1')
    .replace(/<@[!&]?\d+>/g, MENTION_SENTINEL);
  return escapeDiscordMarkdown(stripped)
    .replaceAll(MENTION_SENTINEL, '[mention]')
    .slice(0, 500);
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

// Global per-sendId mutex for the "Add Recipients" flow. Each /qurl send
// has a unique sendId, so in today's code paths the per-closure flag below
// already prevents double-entry. This Map is belt-and-suspenders: if a
// future refactor ever shares a sendId across contexts (e.g. bot restart
// loads unfinished sends from DB), the global lock still holds.
const addRecipientsLocks = new Set();

const sendCooldowns = new Map();

// Per-userId generation counter for the DM late-drop catcher. Each
// time the 60s file-capture window times out, we arm a fire-and-
// forget 5-min awaitMessages on the DM channel. discord.js
// MessageCollectors do NOT consume — multiple stale catchers from
// repeated timeouts (default cooldown 30s, capture window 60s, so
// stacking is realistic) would all observe the same late drop and
// each fire a "60 seconds expired" reply. Counter approach: each
// new catcher captures the post-increment value and bails in its
// .then() if a newer catcher has been armed. The Map is cleaned in
// the catcher's .then() (whether or not it fires the reply); a
// stale catcher whose .then() never fires before the user is
// forgotten will just see a future (unrelated) catcher's increment
// and bail naturally on its own resolution.
//
// No eviction ceiling here (unlike sendCooldowns above): the Map is
// bounded to at most one entry per user with an active catcher,
// because each new arm both increments the user's entry and the
// catcher's terminal .then()/.catch() deletes it. A user with a
// pending catcher contributes exactly one entry; a user with no
// active catcher contributes zero. So the steady-state size is
// bounded by the number of users currently inside their 5-min
// late-drop window, which is naturally small.
const lateDropGenerations = new Map();
// Hard ceiling so a bad actor spraying unique user IDs (or a bug generating
// them) can't grow the Map beyond this. Above this, the 10%-drop eviction
// still fires; if that fails to reclaim, setCooldown drops the oldest
// single entry to guarantee we never exceed the cap.
const SEND_COOLDOWNS_MAX = 20000;

function isOnCooldown(userId) {
  const last = sendCooldowns.get(userId);
  if (!last) return false;
  return Date.now() - last < config.QURL_SEND_COOLDOWN_MS;
}

function setCooldown(userId) {
  // LRU-ish behavior: delete first so set() re-inserts at the end of the
  // Map's insertion order. Combined with the bulk 10%-drop eviction below,
  // active users stay resident while stale entries roll out.
  sendCooldowns.delete(userId);
  sendCooldowns.set(userId, Date.now());
  if (sendCooldowns.size > 10000) {
    const dropCount = Math.max(1, Math.floor(sendCooldowns.size / 10));
    const it = sendCooldowns.keys();
    for (let i = 0; i < dropCount; i++) {
      const k = it.next().value;
      if (k === undefined) break;
      sendCooldowns.delete(k);
    }
  }
  // Belt-and-suspenders: guarantee the hard cap even if the bulk drop
  // didn't reclaim enough (pathological insertion patterns).
  while (sendCooldowns.size > SEND_COOLDOWNS_MAX) {
    const oldest = sendCooldowns.keys().next().value;
    if (oldest === undefined) break;
    sendCooldowns.delete(oldest);
  }
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
//   4. 'Someone' — last-resort literal so callers never get null.
// Optional chains throughout so a malformed interaction (no user, no
// member) returns 'Someone' instead of throwing inside DM-dispatch.
// Used by both the DM embed and the channel announcement so a single
// send always shows the same name in both places.
function resolveSenderAlias(interaction) {
  return interaction?.member?.displayName
    ?? interaction?.user?.displayName
    ?? interaction?.user?.username
    ?? 'Someone';
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

// --- Shared DM delivery payload builder ---
// Builds the {embeds, components} payload for a per-recipient DM. The
// embed copy is intentionally evocative ("opened a door", "Door closes")
// rather than literal ("shared a file with you") — the brand goal is to
// convey the qURL hidden-layer model, not just announce a file transfer.
// The qURL link is rendered as a `🔗 Step Through` Link button rather
// than a bare URL field; recipients click the button to open the link
// in their default browser.
//
// `senderAlias` is the sender's friendly display name (Discord nickname
// > globalName > username) — same source resolveSenderAlias used at the
// channel-announce site, so the alias shown matches across both surfaces.
// `personalMessage` is optional caller-provided context; if present, it
// renders as an italicized blockquote above the body paragraph.
//
// Returns the full Discord message options object (`embeds` + `components`)
// rather than just the embed, since the button is not part of the embed
// — it lives in a top-level component row alongside it. Callers pass the
// returned payload directly to `sendDM`.
//
// Example of what the recipient sees:
//
//     ┌─────────────────────────────────────────────────────────────┐
//     │  qURL · APP · Today at 2:47 PM                              │  (Discord-rendered header)
//     │                                                             │
//     │  Vik opened a door for you.                                 │  (description)
//     │                                                             │
//     │  > "Quarterly numbers — for your eyes only."                │  (optional personal message — italic blockquote)
//     │                                                             │
//     │  🕐 Door closes in 1 day                                    │  (Discord <t:N:R> — auto-updates client-side
//     │                                                             │   to "in 16 hours" / "in 1 hour" / "1 hour ago")
//     │                                                             │
//     │  Quantum URL (qURL) · The internet has a hidden layer.     │  (final embed field;
//     │  This is how you enter.                                     │   `qURL` → https://layerv.ai)
//     │                                                             │
//     │  ┌──────────────────────────┐                               │
//     │  │   🔗 Step Through        │  (Link button — opens qURL)
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

// Per-pick cap on UserSelectMenuBuilder.setMaxValues. Discord's hard
// limit is 25; capping at 10 bounds the UX. Both the initial user-
// target select AND the channel-target's "Add more recipients" flow
// use this — keep them in lockstep so a future bump doesn't drift.
const USER_SELECT_PER_PICK_CAP = 10;

// Shared Google Maps URL patterns. Both `/qurl send`'s modal-driven
// location-text capture and `/qurl file`/`/qurl map`'s slash-option
// `location:` consume these — extracted to a single source so a
// future "new URL shape" tweak only happens here.
//
// Each pattern bounds character-class repetition (`{1,500}`, `{1,32}`,
// etc.) to keep ReDoS-resistant against pathological inputs.
const MAPS_URL_PATTERNS = [
  /https?:\/\/(?:www\.)?google\.com\/maps\/(?:place|search|dir|@)[\w/.,@?=&+%-]{1,500}/,
  /https?:\/\/(?:goo\.gl\/maps|maps\.app\.goo\.gl)\/[\w-]{1,100}/,
  /https?:\/\/(?:www\.)?google\.com\/maps\/embed\/v1\/\w{1,32}\?[^\s]{1,500}/,
];

// decodeURIComponent throws URIError on malformed %-encoding (e.g. %ZZ).
// Swallow + return the raw string — a garbled label is preferable to a
// crashed command handler.
function safeDecodeURIComponent(s) {
  try { return decodeURIComponent(s); } catch { return s; }
}

// Parse a free-form `location:` input into `{ locationUrl, locationName }`.
// Detection order:
//   1. Google Maps URL embedded anywhere in the input → preserve URL,
//      extract name from `?q=` or `/place/<name>`.
//   2. Anything else → synthesize a `/maps/search/<input>` URL with the
//      whole input as the name.
//
// Returns BOTH fields; caller is responsible for escaping the name
// (markdown injection defense) and applying any cap. The two call
// sites — `handleSend`'s modal branch and `handleQurlMap`'s slash
// entry — share this so a pattern tweak lands in one place.
function parseLocationInput(rawInput) {
  let detectedUrl = null;
  for (const pattern of MAPS_URL_PATTERNS) {
    const match = rawInput.match(pattern);
    if (match) { detectedUrl = match[0]; break; }
  }
  if (detectedUrl && isGoogleMapsURL(detectedUrl)) {
    const queryMatch = detectedUrl.match(/[?&]q=([^&]+)/);
    const placeMatch = detectedUrl.match(/\/place\/([^/@]+)/);
    let name = null;
    if (queryMatch) name = safeDecodeURIComponent(queryMatch[1].replace(/\+/g, ' '));
    else if (placeMatch) name = safeDecodeURIComponent(placeMatch[1].replace(/\+/g, ' '));
    return { locationUrl: detectedUrl, locationName: name };
  }
  // Pre-extraction the legacy /qurl send handler had TWO separate
  // branches here ("detectedUrl but not Google Maps" vs "no
  // detectedUrl"), each producing the same synthesized-search URL +
  // raw-input name. The consolidated single branch below is
  // intentional, NOT dead code — both prior cases collapse to the
  // same behavior, so a maintainer is welcome to leave the
  // simplification as-is.
  return {
    locationUrl: `https://www.google.com/maps/search/${encodeURIComponent(rawInput)}`,
    locationName: rawInput,
  };
}

function buildDeliveryPayload({ senderAlias, qurlLink, expiresAt, personalMessage }) {
  // sanitizeDisplayName: NFKC + bidi/zero-width strip + markdown escape
  // + 64-char cap + 'Someone' fallback. Same helper used at the channel
  // announcement site so the spoof defense doesn't drift between sites.
  const safeSender = sanitizeDisplayName(senderAlias);

  const embed = new EmbedBuilder()
    .setColor(COLORS.QURL_BRAND)
    .setDescription(`**${safeSender}** opened a door for you.`);

  if (personalMessage) {
    // CONTRACT: `personalMessage` arrives pre-sanitized — handleSend pipes
    // raw input through `sanitizeMessage` (markdown escape + @-mention
    // strip) before constructing this payload, and the addRecipients
    // path reads from `sendConfig.personal_message` which was sanitized
    // at write time. Raw interpolation into the template below is safe
    // ONLY because of that upstream pass. A future caller that bypasses
    // sanitizeMessage (or a DB row read that skips re-sanitize) would
    // silently regress to markdown injection — keep the contract.
    //
    // Discord blockquote (`> `) only quotes one line and italic (`*…*`)
    // does not span newlines, so a multi-line message would render with
    // only the first line styled. Flatten newlines to a space so the
    // recipient sees one tidy quote — matches the design mockup which
    // shows the message as a single-line styled box. 280-char cap keeps
    // the embed visually compact now that the fixed body copy is gone.
    const capped = personalMessage.substring(0, 280).replace(/[\r\n]+/g, ' ').trim();
    embed.addFields({ name: '\u200B', value: `> *"${capped}"*` });
  }

  embed.addFields(
    {
      // Discord's native relative-time markdown: <t:UNIX:R> renders
      // CLIENT-SIDE based on the viewer's current time, so the recipient
      // sees "in 1 day" at send time, "in 16 hours" 8 hours later, and
      // "1 hour ago" once the link has expired. No bot-side editing
      // needed — Discord handles the live update.
      //
      // Fail-loud on a missing/invalid expiresAt rather than rendering
      // literal "<t:undefined:R>" or "<t:NaN:R>" to a recipient. Matches
      // the contract-violation throw in handleAddRecipients (same fail-
      // loud-over-silent-degradation principle).
      name: '\u200B',
      value: (() => {
        if (!Number.isFinite(expiresAt)) {
          throw new Error(`buildDeliveryPayload: expiresAt must be a finite Unix-seconds number (got ${expiresAt})`);
        }
        return `\ud83d\udd50 Door closes <t:${expiresAt}:R>`;
      })(),
    },
    {
      // Brand line lives in a regular embed field (NOT setFooter) because
      // Discord embed footers are plain-text only and we need the markdown
      // hyperlink on `qURL` to point at https://layerv.ai. The brand line
      // anchors the recipient back to the qURL product page.
      name: '\u200B',
      value: 'Quantum URL ([qURL](https://layerv.ai)) · The internet has a hidden layer. This is how you enter.',
    },
  );

  // Link button: opens qurlLink in the recipient's browser on a
  // single click. No interaction handler needed — Discord handles
  // the redirect.
  //
  // Style is `ButtonStyle.Link` because Discord requires URL buttons
  // to be Link-style (Primary/Success/Danger/Secondary cannot carry
  // a URL — they only fire interaction handlers). Link buttons render
  // gray, which can read as "text" rather than a clickable affordance.
  // The leading 🔗 emoji adds visual weight so the recipient sees a
  // clear button shape; arrow `→` dropped from the label since the
  // emoji conveys the same "go elsewhere" intent.
  const stepThrough = new ButtonBuilder()
    .setStyle(ButtonStyle.Link)
    .setEmoji('🔗')
    .setLabel('Step Through')
    .setURL(qurlLink);
  const components = [new ActionRowBuilder().addComponents(stepThrough)];

  return { embeds: [embed], components };
}

// --- Link status monitor ---
// Track live monitors so a burst of /qurl send commands can't stack more
// than MAX_CONCURRENT_MONITORS setIntervals. When we cross the cap, the
// oldest monitor is stopped to make room (the user can still `/qurl revoke`;
// they just stop seeing live status updates in the original message).
const activeMonitors = new Set();

function monitorLinkStatus(sendId, interactionArg, qurlLinksArg, recipientsArg, expiresIn, baseMsg, buttonRowArg, delivered, apiKey) {
  // Rebind params as closure-mutable so stop() can null them out for GC.
  // Long-running monitors (up to 1h × MAX_CONCURRENT_MONITORS=50) otherwise
  // pin interaction/recipients/buttonRow in the setInterval closure.
  let interaction = interactionArg;
  let qurlLinks = qurlLinksArg;
  let recipients = recipientsArg;
  let buttonRow = buttonRowArg;
  void buttonRow; // buttonRow is used transitively via setComponents inline; keep alias for symmetry
  // Returns a control object: call monitor.addRecipients(count) when new recipients are added
  let currentBaseMsg = baseMsg;
  let stopped = false;
  let timer = null; // assigned after control object
  const control = {
    addRecipients(count, newResourceIds) {
      expectedCount += count;
      if (newResourceIds) {
        for (const rid of newResourceIds) {
          if (!resourceIds.includes(rid)) resourceIds.push(rid);
        }
      }
      trackedQurlIds = null;
      trackingGeneration++;
    },
    stop() {
      stopped = true;
      clearInterval(timer);
      activeMonitors.delete(control);
      // Release references on the closures; over many sends these would
      // otherwise accumulate. trackedQurlIds may already be null (see
      // addRecipients — null forces a re-init next tick).
      linkStatus.clear();
      if (trackedQurlIds) trackedQurlIds.clear();
      trackedQurlIds = null;
      // Drop the big closure-captured objects so GC can reclaim them — an
      // idle timer that was already cleared above holds the closure frame
      // alive until the control object itself is GC'd.
      interaction = null;
      qurlLinks = null;
      recipients = null;
      buttonRow = null;
    },
    updateBaseMsg(msg) {
      currentBaseMsg = msg;
    },
    getFullMsg() {
      return buildStatusMsg();
    },
  };
  const expiryMs = expiryToMs(expiresIn);
  // Cap monitor lifetime at 1h regardless of link expiry. Links still expire
  // normally at the API level; we just stop updating the Discord message
  // after an hour to avoid multi-day setIntervals for 7d expiries.
  const MAX_MONITOR_DURATION_MS = 60 * 60 * 1000;
  const maxMonitorMs = Math.min(expiryMs + 60000, MAX_MONITOR_DURATION_MS);

  const resourceIds = [...new Set(qurlLinks.map(l => l.resourceId))];
  let expectedCount = delivered;

  // Track status per qurl_id: { status, username }
  const linkStatus = new Map();
  let trackedQurlIds = null;
  // Monotonic counter bumped by addRecipients(). A tick that begins init
  // while generation=N and finishes after generation advanced to N+1 must
  // abort — its localSet was built against pre-add resourceIds/expectedCount.
  let trackingGeneration = 0;
  let allDone = false;

  const pollInterval = Math.max(15000, Math.min(60000, expiryMs / 10));
  const startTime = Date.now();


  function buildStatusMsg() {
    let opened = 0, expired = 0, pending = 0;
    for (const s of linkStatus.values()) {
      if (s.status === 'opened') opened++;
      else if (s.status === 'expired') expired++;
      else pending++;
    }
    let msg = currentBaseMsg;
    if (linkStatus.size > 0) {
      msg += '\n\n**Link Status:**';
      if (opened > 0) msg += `\n\u2705 ${opened} of ${linkStatus.size} opened`;
      if (expired > 0) msg += `\n\u23f0 ${expired} expired`;
      if (pending > 0) msg += `\n\u23f3 ${pending} pending`;
      if (pending === 0) msg += `\n\n\u2714\ufe0f **All ${linkStatus.size} links resolved**`;
    }
    return msg;
  }

  let pollCount = 0;
  // INVARIANT: `trackedQurlIds` starts null and is populated on the first tick
  // (see line ~277). control.addRecipients() sets it back to null mid-tick so
  // the NEXT tick re-initializes tracking against the new link set. Any read
  // of `trackedQurlIds` inside this callback that happens AFTER an `await`
  // must first re-check `if (!trackedQurlIds) return;` because addRecipients
  // can null it during any awaited gap. If you add new awaits here, guard
  // subsequent reads accordingly.
  timer = setInterval(async () => {
    pollCount++;
    if (pollCount > 20 && pollCount % 4 !== 0) return;
    else if (pollCount > 5 && pollCount % 2 !== 0) return;
    if (stopped || allDone || Date.now() - startTime > maxMonitorMs) {
      clearInterval(timer);
      // interaction may have been nulled by stop() if this callback was
      // already mid-execution when stop fired — clearInterval only prevents
      // FUTURE ticks, not the one currently running. Skip the final edit
      // in that case; the closure refs are already being released.
      if (!interaction) return;
      const finalMsg = buildStatusMsg() + '\n(Use `/qurl revoke` to revoke later)';
      await interaction.editReply({ content: finalMsg, components: [] }).catch(logIgnoredDiscordErr);
      return;
    }
    try {
      let changed = false;

      // Initialize tracking on first poll. A single send may span multiple
      // resources (both file sends >TOKENS_PER_RESOURCE, which re-upload in
      // batches of 10, and location sends which mint one qurl per resource),
      // so walk every resource and collect all its qurls, then pick the
      // `expectedCount` most recent across the whole send.
      if (!trackedQurlIds) {
        // Snapshot the generation BEFORE the await. If addRecipients fires
        // during Promise.all it will bump the counter and the post-await
        // check will abort this init — the new tick must re-read
        // resourceIds/expectedCount against the updated state.
        const genAtStart = trackingGeneration;
        const snapshotResourceIds = resourceIds.slice();
        const snapshotExpectedCount = expectedCount;
        const localSet = new Set();
        const statuses = await Promise.all(
          snapshotResourceIds.map(rid => getResourceStatus(rid, apiKey).catch(() => null))
        );
        if (trackingGeneration !== genAtStart) {
          // addRecipients ran during the await; the snapshot is stale.
          // Leave trackedQurlIds null so the next tick re-inits fresh.
          return;
        }
        const allQurls = [];
        for (const data of statuses) {
          if (!data || !data.qurls) continue;
          for (const q of data.qurls) allQurls.push(q);
        }
        // Secondary sort on qurl_id makes the order deterministic when two
        // qurls land in the same created_at millisecond — otherwise the
        // positional recipient→qurl mapping becomes non-deterministic.
        allQurls.sort((a, b) => {
          const d = new Date(a.created_at) - new Date(b.created_at);
          if (d !== 0) return d;
          return String(a.qurl_id).localeCompare(String(b.qurl_id));
        });
        const recentN = allQurls.slice(-snapshotExpectedCount);
        if (recentN.length !== recipients.length) {
          logger.warn('Monitor tracking count mismatch', {
            sendId, qurls: recentN.length, recipients: recipients.length,
          });
        }
        // Bound the zip to min(qurls, recipients) so we never index recipients
        // past its end or leave a qurl without a label. Any excess on either
        // side is logged above for oncall diagnosis.
        const trackCount = Math.min(recentN.length, recipients.length);
        for (let i = 0; i < trackCount; i++) {
          const q = recentN[i];
          localSet.add(q.qurl_id);
          const username = recipients[i] ? recipients[i].username : `user-${i + 1}`;
          linkStatus.set(q.qurl_id, { status: 'pending', username });
        }
        trackedQurlIds = localSet;
        logger.info('Link monitor tracking', { sendId, tracked: trackedQurlIds.size, resources: resourceIds.length });
      }

      // Poll all resources for status changes (parallel to avoid N sequential API calls per tick)
      const pollResults = await Promise.all(
        resourceIds.map(rid => getResourceStatus(rid, apiKey).catch(() => null))
      );
      // control.addRecipients() can null out trackedQurlIds during the await
      // above — skip this tick and let the next one re-init the tracking set.
      if (!trackedQurlIds) return;
      for (const data of pollResults) {
        if (!data || !data.qurls) continue;
        for (const qurl of data.qurls) {
          if (!trackedQurlIds.has(qurl.qurl_id)) continue;
          const current = linkStatus.get(qurl.qurl_id);
          if (!current) continue;
          if (qurl.use_count > 0 && current.status !== 'opened') {
            linkStatus.set(qurl.qurl_id, { ...current, status: 'opened' }); changed = true;
          } else if (qurl.status === 'expired' && current.status === 'pending') {
            linkStatus.set(qurl.qurl_id, { ...current, status: 'expired' }); changed = true;
          }
        }
      }
      if (changed) {
        // Same race as above — stop() may have nulled interaction during the
        // awaited Promise.all. Skip the edit; next tick's stopped-check exits.
        if (!interaction) return;
        const pending = [...linkStatus.values()].filter(s => s.status === 'pending').length;
        await interaction.editReply({ content: buildStatusMsg(), components: pending > 0 ? [buttonRow] : [] }).catch(logIgnoredDiscordErr);
        if (pending === 0) { allDone = true; clearInterval(timer); }
      }
    } catch (err) {
      logger.error('Link monitor poll failed', { sendId, error: err.message });
    }
  }, pollInterval);
  if (timer && timer.unref) timer.unref();

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

// --- Guild member fetch cache (30s TTL, per-guild). Also coalesces concurrent
// callers for the same guild so two simultaneous /qurl send commands don't
// each fire a separate fetch.
const memberFetchCache = new Map(); // guildId -> { timestamp, inFlight }
// 60s TTL. guild.members.fetch() is slow on large guilds (rate-limited,
// paginates) so a longer cache reduces latency on repeat /qurl send calls
// within the same minute. 60s is still short enough that a user who just
// joined/left won't be missed for long.
//
// Intent dependency: this fetch relies on the GuildMembers privileged
// intent (declared in src/discord.js — boot-time assertion guards against
// removal). With that intent, guild.members.fetch() handles gateway
// pagination transparently and returns the full member list regardless
// of guild size. Without it, the call is rejected by Discord. The
// 100-guild verification gate is for the GuildMembers intent itself
// (the "Server Members Intent" toggle in the dev portal) — not a
// per-call cap.
const MEMBER_FETCH_TTL = 60000;
async function fetchGuildMembers(guild) {
  const now = Date.now();
  const entry = memberFetchCache.get(guild.id);
  // Decide both checks inside a single `if (entry)` branch so a concurrent
  // caller can't observe the entry AFTER inFlight cleared but BEFORE a new
  // one was set, slipping through both gates and firing a duplicate fetch.
  // Order: inFlight (join it) → TTL (cache hit, skip) → fall through (refresh).
  if (entry) {
    if (entry.inFlight) return entry.inFlight;
    if (now - entry.timestamp < MEMBER_FETCH_TTL) return;
  }
  const promise = guild.members.fetch();
  memberFetchCache.set(guild.id, { timestamp: now, inFlight: promise });
  try {
    await promise;
    // Only stamp success — on failure, drop the entry so the next caller
    // retries immediately instead of sitting on a stale-or-empty member list
    // for the full TTL window.
    memberFetchCache.set(guild.id, { timestamp: Date.now(), inFlight: null });
  } catch (err) {
    memberFetchCache.delete(guild.id);
    throw err;
  }
}

// --- /qurl send handler ---
// TODO(#55): Split commands.js into focused modules — see https://github.com/layervai/qurl-integrations/issues/55
//
// REVIEW NOTE: The size of commands.js is tracked as a follow-up in
// issue #55 and is OUT OF SCOPE for this PR. Reviewers (human or bot)
// should NOT re-flag this file's length — the split will land in its
// own PR against a stable baseline.

/**
 * Mint one-time links across a stream of connector resources, each capped at
 * TOKENS_PER_RESOURCE tokens. When a resource is exhausted, the caller's
 * `reuploadFn` is invoked to produce a new one.
 *
 * Extracted from four near-identical copies across handleSend (file/location)
 * and handleAddRecipients (file/location). Centralizing this means a fix to
 * the re-upload / batching / quota logic lands in one place.
 *
 * @param {object} opts
 * @param {string} opts.initialResourceId — resource_id from the first upload
 * @param {() => Promise<{resource_id: string}>} opts.reuploadFn — called when
 *   the current resource's token pool is drained. Must return a fresh resource.
 * @param {string} opts.expiresAt — ISO string; forwarded to mintLinks.
 * @param {number} opts.recipientCount — number of tokens to mint in total.
 * @param {string} opts.apiKey — QURL API key.
 * @returns {Array<{qurl_link: string, resourceId: string}>}
 */
async function mintLinksInBatches({ initialResourceId, reuploadFn, expiresAt, recipientCount, apiKey }) {
  const allLinks = [];
  let currentResourceId = initialResourceId;
  let tokensUsed = 0;

  for (let i = 0; i < recipientCount; i += TOKENS_PER_RESOURCE) {
    if (tokensUsed >= TOKENS_PER_RESOURCE && i > 0) {
      const re = await reuploadFn();
      currentResourceId = re.resource_id;
      tokensUsed = 0;
    }
    const batchSize = Math.min(TOKENS_PER_RESOURCE, recipientCount - i);
    const minted = await mintLinks(currentResourceId, expiresAt, batchSize, apiKey);
    for (const link of minted) {
      allLinks.push({ qurl_link: link.qurl_link, resourceId: currentResourceId });
    }
    tokensUsed += batchSize;
  }
  return allLinks;
}

// executeSendPipeline — back-half of the /qurl send lifecycle. The
// destructure signature is the authoritative param surface; the
// notes below capture only non-obvious contract guarantees that a
// reader couldn't infer from the call sites alone.
//
// Entry gates (fire-and-forget cancel-edit + throw):
//   - `isVoiceContext` must be a strict boolean.
//   - `target` must be `'user'` or `'channel'`.
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
//   - `target` is `'user' | 'channel'` ONLY — voice/text channel
//     discrimination is on `isVoiceContext`, NOT on `target`.
//     Setting `target: 'voice'` would silently suppress the
//     channel-announce (the gate at the announce site is strict
//     equality on `'channel'`).
//
//   - `isVoiceContext` is REQUIRED and strictly validated as a
//     boolean (see entry-point assertion). A silent default would
//     mis-render the channel-announce blurb for a voice-context
//     send whose flag got dropped in serialization — exactly the
//     silent-regression shape every other param avoids by landing
//     in a grep-discoverable DB column or failing loudly inside
//     the upload/mint stack.
//
// Required `interaction` surface (fail loudly at the corresponding
// call site if missing):
//
//   - `interaction.user.id`            (cooldown key + senderDiscordId
//                                       on every DB row + audit event)
//   - `interaction.channelId`          (qurl_sends.channel_id)
//   - `interaction.channel`            (null-guarded; nullable just
//                                       drops the channel-announce)
//   - `interaction.member?.displayName` + `user.username` fallback
//                                      (resolveSenderAlias → DM embed
//                                       + channel-announce wording)
//   - `interaction.guild?.members?.fetch` (best-effort, recipient-
//                                       alias resolution for cold cache)
//   - `interaction.editReply`          (every user-visible status
//                                       update on the primary message)
//   - `interaction.channel.send`       (only on `target === 'channel'
//                                       && delivered > 0`; logged-
//                                       and-swallowed on failure)
//
// Resolved value is unused — both terminal completion and the
// early-exit `return interaction.editReply(...)` paths discard
// observable result; the function exists for its user-visible
// side-effects (editReply + DM fan-out + channel-announce).
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
  // this array (see docstring's "transferred ownership" note).
  recipients,
  target,
  // REQUIRED — validated at entry (see assertion below). NO default
  // value: an omitted-or-non-boolean caller fails loudly instead of
  // silently landing on text-channel wording for a voice-context send.
  isVoiceContext,
  expiresIn,
  selfDestructSeconds,
  personalMessage,
  sendNonce,
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

  // `isVoiceContext` strict-boolean gate. Unique among the params
  // because a wrong value surfaces only as a subtle copy mismatch
  // in the non-ephemeral channel-announce — the silent-regression
  // shape every other input either lands in a grep-discoverable
  // DB column or fails loudly inside the upload/mint stack. The
  // clearCooldown ahead of the throw matches handleSend's "clear
  // on every error path" convention so a caller that hit the
  // gate doesn't strand the user in a cooldown window.
  if (typeof isVoiceContext !== 'boolean') {
    // `typeof=` + `value=` rendered separately because
    // `typeof null === 'object'` is a classic foot-gun in
    // a single-field "(got object: null)" rendering. truncForLog
    // keeps the message bounded if a future caller hands a
    // pathological value (1MB string, etc.) and appends a `…`
    // marker so a prod-log reader can tell a truncated rendering
    // from the full payload.
    failGate(TypeError, `executeSendPipeline: isVoiceContext must be a boolean (got typeof=${typeof isVoiceContext}, value=${truncForLog(isVoiceContext)})`);
  }

  // `target` allowed-set gate. Same silent-mis-render shape: an
  // unrecognized value (e.g. `'voice'`) would silently suppress
  // the channel-announce — the announce site gates on strict
  // equality with `'channel'`.
  if (target !== 'user' && target !== 'channel') {
    failGate(TypeError, `executeSendPipeline: target must be 'user' or 'channel' (got ${truncForLog(target)})`);
  }

  // Defense-in-depth SSRF re-check. handleSend's Step-2 validates
  // attachment.url against isAllowedSourceUrl BEFORE calling the
  // pipeline; this gate catches a future caller that forgets.
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

  // `expiresIn` allowed-set gate. Today only handleSend's
  // expiry-select dropdown can supply it, so the set is closed.
  // A future caller reconstructing from a persisted payload could
  // ship an off-set value (`'25h'`, `'bogus'`) that lands in the
  // DB and trips downstream when `expiryToISO` / `expiryToMs`
  // hit it. Validate at the boundary instead.
  if (!Object.prototype.hasOwnProperty.call(EXPIRY_LABELS, expiresIn)) {
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
  // ≤ QURL_SEND_MAX_RECIPIENTS" contract is enforced by handleSend's
  // front-half today; this is defense-in-depth for a future caller
  // (deserialized payload, programmatic retry, admin tool) that
  // skips those checks. Trips here would otherwise surface deep
  // inside mintLinksInBatches as "Failed to create any links" with
  // no caller-side breadcrumb. The non-empty check ALSO fences the
  // chain of `recipients.length` reads that follow (the cap check
  // below, then the editReply) — a non-array reaching either site
  // would crash on `.length` lookup against undefined.
  if (!Array.isArray(recipients) || recipients.length === 0) {
    // Same canonical `typeof=`, `value=` rendering as the
    // isVoiceContext gate above. Empty-array case renders the literal
    // `<empty array>` sentinel (truncForLog on `[]` would `String()`
    // to the empty string, which a prod-log reader couldn't
    // distinguish from a missing value-field).
    const detail = Array.isArray(recipients)
      ? 'typeof=object, value=<empty array>'
      : `typeof=${typeof recipients}, value=${truncForLog(recipients)}`;
    failGate(TypeError, `executeSendPipeline: recipients must be a non-empty array (got ${detail})`);
  }
  if (recipients.length > config.QURL_SEND_MAX_RECIPIENTS) {
    failGate(RangeError, `executeSendPipeline: recipients.length (${recipients.length}) exceeds QURL_SEND_MAX_RECIPIENTS (${config.QURL_SEND_MAX_RECIPIENTS})`);
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
        resourceId: allLinks[i].resourceId,
      }));
      logger.audit(AUDIT_EVENTS.UPLOAD_SUCCESS, { send_id: sendId, kind: 'file' });
    } else {
      // Location send — upload JSON payload to connector, then mint in batches
      // of TOKENS_PER_RESOURCE and re-upload when the pool is drained.
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
      });

      if (allLinks.length < recipients.length) {
        logger.error('mintLinks returned fewer links than expected for location', { expected: recipients.length, got: allLinks.length });
        clearCooldown(interaction.user.id);
        return interaction.editReply({ content: `Only ${allLinks.length} of ${recipients.length} links could be created. Please try again.` });
      }

      qurlLinks = recipients.map((r, i) => ({
        recipientId: r.id,
        qurlLink: allLinks[i].qurl_link,
        resourceId: allLinks[i].resourceId,
      }));
      logger.audit(AUDIT_EVENTS.UPLOAD_SUCCESS, { send_id: sendId, kind: 'location' });
    }
  } catch (error) {
    logger.error('Failed to prepare QURL links', { error: error.message, apiCode: error.apiCode });
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

  // Persist ALL links to DB BEFORE sending DMs. If the write fails the links
  // still exist on the QURL side but there's no local record to revoke them
  // later — abort the send and surface the error instead of continuing to DMs.
  try {
    await db.recordQURLSendBatch(qurlLinks.map(link => ({
      sendId, senderDiscordId: interaction.user.id, recipientDiscordId: link.recipientId,
      resourceId: link.resourceId, resourceType, qurlLink: link.qurlLink,
      expiresIn, channelId: interaction.channelId, targetType: target,
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

  // Send DMs
  let delivered = 0;
  let failed = 0;
  const failedUsers = [];
  const recipientMap = new Map(recipients.map(r => [r.id, r]));

  // Compute the absolute expiry instant once for this dispatch (Unix
  // seconds — Discord's <t:N:R> format requires seconds, not millis).
  // Using send-time + duration rather than reading from the API mint
  // response since `mintLinks` doesn't currently surface `expires_at`.
  // Drift between this clock and the API's enforcement clock is bounded
  // by the time between this line and the mint call (sub-second on
  // handleSend; can be a few seconds on handleAddRecipients which
  // re-downloads + re-uploads + re-mints first). Negligible at the
  // 30m–7d horizon — recipients see "in 24 hours" instead of
  // "in 23h 59m 56s" on the worst-case path.
  const expiresAt = Math.floor((Date.now() + expiryToMs(expiresIn)) / 1000);

  const dmResults = await batchSettled(qurlLinks, async (link) => {
    const recipient = recipientMap.get(link.recipientId);
    // Audit in `finally` so the metric fires for every recipient regardless
    // of where the dispatch fails — sendDM resolving to false, sendDM
    // throwing (against contract — see apps/discord/src/discord.js), OR
    // buildDeliveryPayload throwing (e.g. on a pathological personalMessage).
    // Audit fires BEFORE the DB write so a DDB-layer throw can't suppress
    // it either — that's the failure mode the audit metric exists to
    // measure. Coverage spans the entire dispatch attempt, not just the
    // network leg.
    let sent = false;
    try {
      const dmPayload = buildDeliveryPayload({
        // member.displayName resolves to nickname || globalName || username,
        // so it works whether the sender has a per-guild nickname, only a
        // global display name, or just the legacy @-handle.
        senderAlias: resolveSenderAlias(interaction),
        qurlLink: link.qurlLink,
        expiresAt,
        personalMessage,
      });
      sent = await sendDM(link.recipientId, dmPayload);
    } finally {
      logger.audit(sent === true ? AUDIT_EVENTS.DISPATCH_SENT : AUDIT_EVENTS.DISPATCH_FAILED, { send_id: sendId });
    }
    await db.updateSendDMStatus(sendId, link.recipientId, sent ? DM_STATUS.SENT : DM_STATUS.FAILED);
    return { recipientId: link.recipientId, username: recipient?.username, sent };
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
    delivered, expiresIn,
    failedNamesPlain, successNames, showAll,
  });

  const confirmRendered = buildConfirmMsg(false);
  let confirmMsg = confirmRendered.content;
  // In attachment mode the file IS the full list, so suppress the
  // Show All toggle — the same shape the post-revoke flow uses.
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
        .setLabel('Show All')
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
  const initialPayload = {
    content: confirmMsg,
    components: delivered > 0 ? [buttonRow] : [],
  };
  if (confirmRendered.attachmentText) {
    initialPayload.files = [
      new AttachmentBuilder(Buffer.from(confirmRendered.attachmentText, 'utf8'), { name: 'recipients.txt' }),
    ];
  }
  const response = await interaction.editReply(initialPayload);

  // Non-ephemeral channel notification when sending to "Everyone" (channel
  // target) or "Voice users" (voice target). The sender's ephemeral reply
  // confirms the send to THEM; this message is what recipients see in the
  // channel so they know to look for the Qurl Bot DM. Without this, a
  // passive channel member who missed the DM ping has no signal that a
  // send happened. Logged-and-swallowed on failure — a missing
  // "Send Messages" permission in a customer server shouldn't fail the
  // whole send (DMs already went out successfully).
  //
  // Guard on `interaction.channel` being present: for a slash command
  // invoked in a guild channel this is always set, but in edge cases
  // (partial-cache on a fresh gateway connect, thread that got
  // archived mid-send, DM-channel dispatch) the channel object can be
  // null — and `.send()` on null throws synchronously, before the
  // try/catch can see it.
  if (interaction.channel && target === 'channel' && delivered > 0) {
    // Same sanitizer the DM embed uses (sanitizeDisplayName: NFKC + bidi/
    // zero-width/control strip + markdown escape + 64-char cap + 'Someone'
    // fallback). Channel-post is a wider blast radius than DM, so applying
    // the same spoof defense here is critical — without it a display name
    // with a leading U+202E flips text direction in the public announcement.
    const safeName = sanitizeDisplayName(resolveSenderAlias(interaction));
    const notifyMsg = isVoiceContext
      ? `📩 **${safeName}** has shared something with users currently connected to this voice channel via **qURL Bot** — check your DMs from qURL Bot.`
      : `📩 **${safeName}** has shared something with all members of this channel via **qURL Bot** — check your DMs from qURL Bot.`;
    try {
      await interaction.channel.send({ content: notifyMsg });
    } catch (err) {
      logger.warn('Failed to send channel notification', { error: err.message });
    }
  }

  logger.info('/qurl send completed', {
    sender: interaction.user.id, sendId, target, resourceType, delivered, failed, expiresIn,
  });

  // Collector handles multiple button clicks (Add Recipients can be clicked multiple times)
  let monitor = null;
  if (delivered > 0) {
    let addRecipientsCount = 0; // Track cumulative adds for cap enforcement
    // Single source of truth for the "adding recipients" lock: the global
    // addRecipientsLocks Set keyed by sendId. Acquire at the top of the
    // collect handler, release in a single outer finally{} so any throw
    // between acquire and the previous dual-flag release paths can't
    // permanently block subsequent button clicks.

    // Create the collector FIRST — if collector setup throws, we return
    // without ever having started the monitor, so no setInterval leaks
    // interaction/recipients/buttonRow/file data in a closure for up to
    // an hour. Only after the collector is established do we kick the
    // monitor setInterval.
    let collector;
    try {
      collector = response.createMessageComponentCollector({
        filter: (i) => i.user.id === interaction.user.id,
        time: TIMEOUTS.QURL_REVOKE_WINDOW,
      });
    } catch (err) {
      logger.error('Failed to create button collector', { sendId, error: err.message });
      return;
    }
    monitor = monitorLinkStatus(sendId, interaction, qurlLinks, recipients, expiresIn, confirmMsg, buttonRow, delivered, apiKey);

    let showAllRecipients = false;

    // `revokeInFlight` dedups concurrent Revoke clicks. `revokeSucceeded`
    // guards the on('end') re-render so a Failed message isn't overwritten
    // by a stale "Revoked 0/0".
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

    collector.on('collect', async (btnInteraction) => {
      if (btnInteraction.customId === `qurl_expand_${sendId}`) {
        await btnInteraction.deferUpdate().catch(logIgnoredDiscordErr);
        showAllRecipients = !showAllRecipients;
        // buildConfirmMsg now returns {content, attachmentText, needsExpand};
        // extract content for the monitor + editReply (string-only).
        confirmMsg = buildConfirmMsg(showAllRecipients).content;
        monitor.updateBaseMsg(confirmMsg);
        const fullMsg = monitor.getFullMsg();
        const updatedRow = new ActionRowBuilder().addComponents(
          new ButtonBuilder().setCustomId(`qurl_add_${sendId}`).setLabel('Add Recipients').setStyle(ButtonStyle.Primary),
          new ButtonBuilder().setCustomId(`qurl_revoke_${sendId}`).setLabel('Revoke All').setStyle(ButtonStyle.Danger),
          new ButtonBuilder().setCustomId(`qurl_expand_${sendId}`).setLabel(showAllRecipients ? 'Show Less' : 'Show All').setStyle(ButtonStyle.Secondary),
        );
        await interaction.editReply({ content: fullMsg, components: [updatedRow] }).catch(logIgnoredDiscordErr);
        return;
      }

      if (btnInteraction.customId === `qurl_revoke_expand_${sendId}`) {
        // Toggle Show All / Show Less on the post-revoke recipient list.
        await btnInteraction.deferUpdate().catch(logIgnoredDiscordErr);
        revokeShowAll = !revokeShowAll;
        const updated = renderRevokeMsg(sendId, revokeResultUserNames, revokeResultTotal, revokeShowAll, revokeResultSuccess);
        await interaction.editReply(revokeReplyPayload(updated)).catch(logIgnoredDiscordErr);
        return;
      }

      if (btnInteraction.customId === `qurl_revoke_${sendId}`) {
        // Sync dedup before any await (Node single-threaded).
        if (revokeInFlight) return btnInteraction.deferUpdate().catch(logIgnoredDiscordErr);
        revokeInFlight = true;
        // Stop monitor BEFORE any editReply — its setInterval can
        // overwrite the revoke-result message otherwise.
        if (monitor) monitor.stop();
        await btnInteraction.deferUpdate().catch(logIgnoredDiscordErr);
        await interaction.editReply({ content: 'Revoking links...', components: [] }).catch(logIgnoredDiscordErr);
        try {
          const revoked = await revokeAllLinks(sendId, interaction.user.id, apiKey);
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
          const initial = renderRevokeMsg(sendId, revokeResultUserNames, revokeResultTotal, false, revokeResultSuccess);
          await interaction.editReply(revokeReplyPayload(initial)).catch(logIgnoredDiscordErr);
          revokeSucceeded = true;
        } catch (err) {
          logger.error('Revoke failed', { sendId, error: err.message });
          await interaction.editReply({
            content: 'Failed to revoke links. Try `/qurl revoke` instead.',
            components: [],
          }).catch(logIgnoredDiscordErr);
          // Reset so the dedup flag isn't sticky if the failure UI
          // ever changes to retain the Revoke button.
          revokeInFlight = false;
        }
        // Collector keeps running for the post-revoke expand toggle;
        // its `time:` window auto-expires.

      } else if (btnInteraction.customId === `qurl_add_${sendId}`) {
        // =====================================================================
        // CRITICAL SECTION — do NOT add any `await` between the three lines
        // below and the next `return` path. Node.js is single-threaded: if
        // check+set+cooldown all happen synchronously, a second button click
        // dispatched to the same handler cannot observe the unlocked state.
        // The `await` in the rejection branches is fine because we've already
        // committed to rejecting at that point.
        // =====================================================================
        // Check-and-claim are now adjacent: if the flag is unset, grab it
        // FIRST (before any cap check), then verify remaining capacity and
        // release on rejection. That way a future refactor that adds an
        // `await` in the remaining check can't reopen a racy window.
        if (addRecipientsLocks.has(sendId)) {
          await btnInteraction.reply({ content: 'Already processing an "Add Recipients" action.', ephemeral: true }).catch(logIgnoredDiscordErr);
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
            });
            return;
          }
          if (isOnCooldown(interaction.user.id)) {
            await btnInteraction.reply({ content: 'Please wait before adding more recipients.', ephemeral: true }).catch(logIgnoredDiscordErr);
            return;
          }
          setCooldown(interaction.user.id);

          // Show user select menu — collect the response on the REPLY message
          const maxSelect = Math.min(USER_SELECT_PER_PICK_CAP, remaining);
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
              // Tell the monitor to track the new links (including new resource IDs for location sends)
              monitor.addRecipients(addResult.delivered, addResult.newResourceIds);
              const totalSent = delivered + addRecipientsCount;
              confirmMsg = `Sent to ${totalSent} user${totalSent !== 1 ? 's' : ''} | Expires: ${expiresIn} | One-time links`;
              if (failed > 0) confirmMsg += `\n${failed} could not be reached`;
              monitor.updateBaseMsg(confirmMsg);
              await interaction.editReply({ content: monitor.getFullMsg(), components: [buttonRow] });
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
      // Stop monitor polling when collector ends for any reason
      if (monitor) monitor.stop();
      if (reason === 'time') {
        // After a SUCCESSFUL revoke, re-render the revoke result
        // instead of the pre-revoke confirmMsg + Management-window
        // banner — otherwise the user's "Revoked X/Y links" view
        // gets clobbered when the collector window ends. Gate on
        // `revokeSucceeded` (not `revokeInFlight`) so a failure
        // message ("Failed to revoke links…") isn't overwritten with
        // a stale "Revoked 0/0 links" line.
        if (revokeSucceeded) {
          // Terminal state: re-render content (Show All may have
          // toggled), strip components. Omit `files`/`attachments`
          // so Discord keeps the existing revoked-users.txt without
          // re-uploading the same blob 15min later.
          const final = renderRevokeMsg(sendId, revokeResultUserNames, revokeResultTotal, revokeShowAll, revokeResultSuccess);
          interaction.editReply({ content: final.content, components: [] }).catch(logIgnoredDiscordErr);
          return;
        }
        // Revoke attempted but failed — leave the failure message.
        if (revokeInFlight) return;
        interaction.editReply({
          content: (monitor ? monitor.getFullMsg() : confirmMsg) + '\n\n⏰ **Management window closed** — use `/qurl revoke` to revoke later.',
          components: [],
        }).catch(logIgnoredDiscordErr);
      }
    });
  }
}

async function handleSend(interaction, apiKey) {
  // awaitMessageComponent below requires a channel handle
  if (!interaction.channel) {
    return interaction.reply({ content: 'Cannot use this command in this context.', ephemeral: true });
  }
  // Defensive: execute() should always pass an apiKey for `send`, but guard
  // in case a future code path calls handleSend directly.
  if (!apiKey) {
    return interaction.reply({ content: 'qURL API key is not configured.', ephemeral: true });
  }

  if (isOnCooldown(interaction.user.id)) {
    return interaction.reply({ content: 'Please wait before sending again.', ephemeral: true });
  }
  // Set cooldown immediately to prevent concurrent request bypass
  setCooldown(interaction.user.id);

  // Hoisted once for reuse across the form (dropdown labels, channel-target
  // resolution, fetchGuildMembers skip) and the back-half (channel-
  // announcement wording). The invocation channel type is fixed for the
  // lifetime of this handler — Discord doesn't reroute live interactions.
  // `interaction.channel` was non-null-guarded above, but read .type
  // through optional chaining as belt-and-suspenders so a future refactor
  // that moves this above the guard doesn't strand a cooldown via a
  // null-deref throw.
  const channelType = interaction.channel?.type;
  const isVoiceContext = (
    channelType === ChannelType.GuildVoice
    || channelType === ChannelType.GuildStageVoice
  );

  const sendNonce = crypto.randomBytes(8).toString('hex');

  // ── Step 1: Initial 2-button reply (Send File / Send Location) ──
  // Replaces the previous 4-options-on-the-slash-command shape per
  // April 2026 customer feedback (the slash popup was confusing —
  // users had to pre-decide file-vs-location AND fill in target
  // before seeing the actual flow). Now: zero options, button-driven
  // wizard. After the button click, the OTHER button disappears
  // because the message is `update`d into the next stage.
  const initRow = new ActionRowBuilder().addComponents(
    new ButtonBuilder()
      .setCustomId(`qurl_init_file_${sendNonce}`)
      .setLabel('\u{1F4C1} Send File')
      .setStyle(ButtonStyle.Primary),
    new ButtonBuilder()
      .setCustomId(`qurl_init_loc_${sendNonce}`)
      .setLabel('\u{1F5FA}\u{FE0F} Send Location')
      .setStyle(ButtonStyle.Primary),
  );

  await interaction.reply({
    content: 'What would you like to send?',
    components: [initRow],
    ephemeral: true,
  });

  let initBtn;
  try {
    initBtn = await interaction.channel.awaitMessageComponent({
      filter: (i) => i.user.id === interaction.user.id &&
        (i.customId === `qurl_init_file_${sendNonce}` || i.customId === `qurl_init_loc_${sendNonce}`),
      time: 60000,
    });
  } catch {
    clearCooldown(interaction.user.id);
    return interaction.editReply({ content: 'No selection made. Send cancelled.', components: [] }).catch(logIgnoredDiscordErr);
  }

  // ── Step 2: Resource collection ──
  // FILE: prompt the user to drop a file in the channel; capture via
  //   awaitMessages with attachment filter. There is no Discord
  //   component that accepts a file upload post-dispatch — only the
  //   slash-command attachment option (which we removed) or this
  //   drop-into-chat pattern can pull a file from the user.
  // LOCATION: open a modal with a single URL/place text input
  //   (modals can ONLY hold TextInput components, so the older shape
  //   stays — minus the optional message and expiry inputs, which
  //   moved to the common final step per the redesign).
  let resourceType;
  let attachment = null;
  let locationUrl = null;
  let locationName = null;

  if (initBtn.customId === `qurl_init_file_${sendNonce}`) {
    resourceType = RESOURCE_TYPES.FILE;

    // Pivot the file capture to a DM with the user when /qurl send was
    // invoked from a guild channel. Dropping the file in the public
    // channel exposes the CDN URL to every channel member — even with
    // an immediate `delete()`, the 1-2s race window leaks the file. A
    // DM between the user and the bot is 1:1 and doesn't have that
    // exposure. If the user invoked /qurl send already in a DM with
    // the bot, just await in the same channel — no pivot needed.
    let captureChannel;
    // Tracks the bot's file-prompt message in the DM so we can delete
    // it after capture. Only set in the DM-pivot path (the DM-already
    // path uses initBtn.update which is the ephemeral interaction
    // reply — no separate DM bot message to clean up).
    let dmPromptMessage = null;
    if (interaction.channel.type === ChannelType.DM) {
      captureChannel = interaction.channel;
      await initBtn.update({
        content: '\u{1F4C1} **Attach a file** — tap **+** to upload (or drag-drop on desktop). I\'ll wait 60 seconds.',
        components: [],
      });
    } else {
      // Try to open a DM and post the prompt there. If the user has
      // server-DMs disabled (DiscordAPIError 50007), bail with a clear
      // next-step message — don't silently fall back to dropping in the
      // guild channel, which is exactly the privacy regression the
      // pivot was added to prevent.
      let dm;
      try {
        dm = await interaction.user.createDM();
        dmPromptMessage = await dm.send('\u{1F4C1} **Ready!** Tap **+** to attach a file (or drag-drop on desktop). I\'ll wait 60 seconds.');
      } catch (err) {
        const dmsBlocked = err && (err.code === 50007 || err.code === '50007');
        if (!dmsBlocked) {
          logger.error('Failed to open DM for file capture', {
            sendNonce, userId: interaction.user.id, error: err?.message, code: err?.code,
          });
        }
        clearCooldown(interaction.user.id);
        return initBtn.update({
          content: dmsBlocked
            ? 'I tried to DM you but your DMs from server members are blocked. Enable them in your privacy settings, or open a DM with me directly and run `/qurl send` there.'
            : 'Could not open a DM right now. Please try again.',
          components: [],
        }).catch(logIgnoredDiscordErr);
      }
      captureChannel = dm;
      await initBtn.update({
        content: '\u{1F4EC} **I sent you a DM — attach your file there and come back here to send it.** I\'ll wait 60 seconds.',
        components: [],
      });
    }

    // awaitMessages — attachments aren't gated by MessageContent intent,
    // and the filter restricts to the invoking user's NEXT message in
    // `captureChannel` bearing an attachment.
    let fileMessage;
    try {
      const messages = await captureChannel.awaitMessages({
        filter: (m) => m.author.id === interaction.user.id && m.attachments.size > 0,
        max: 1,
        time: 60000,
        errors: ['time'],
      });
      fileMessage = messages.first();
    } catch (err) {
      // awaitMessages with errors:['time'] rejects with the collected
      // Collection on timeout (discord.js v14 contract). Anything that's
      // an Error (channel destroyed, permissions revoked mid-await,
      // gateway disconnect) is unexpected and worth a log line so a user
      // reporting "I dropped the file but it didn't take" doesn't go
      // uninvestigated.
      const isUnexpected = err instanceof Error;
      if (isUnexpected) {
        logger.warn('awaitMessages failed unexpectedly during file capture', {
          sendNonce, userId: interaction.user.id, error: err?.message,
        });
      }
      // Tear down the DM prompt — bots can't delete it later, so a
      // leftover orphans in the user's DM forever. Fire-and-forget
      // so a delete failure doesn't mask the user-facing message.
      if (dmPromptMessage) {
        dmPromptMessage.delete().catch((dErr) => logger.warn('Failed to delete stale DM prompt after capture timeout/error', {
          sendNonce, userId: interaction.user.id, error: dErr?.message,
        }));
      }
      // Late-drop catcher: users routinely come back to the DM and drop
      // the file after the 60s window has already passed, then wonder
      // why nothing happened. Set up a fire-and-forget listener for the
      // next 5 min on the DM channel and reply once with a reattach
      // hint if a late attachment arrives. Single-shot (max:1) so it
      // auto-tears down.
      //
      // Two concurrency hazards both apply, both guarded:
      //
      // 1. STACKING — repeated timeouts within 5 min (achievable since
      //    QURL_SEND_COOLDOWN_MS defaults to 30s while the capture window
      //    is 60s) would otherwise arm multiple catchers; a single late
      //    drop would fire each catcher's .then() and produce N reply
      //    messages. lateDropGenerations bumps a per-userId counter on
      //    each arm; each catcher captures its own generation and bails
      //    if a newer one has been armed.
      //
      // 2. RACE WITH FRESH SEND — discord.js MessageCollectors do NOT
      //    consume, so a stale catcher and a fresh /qurl send's 60s
      //    collector both observe the same DM message. Without a guard,
      //    a user who retries inside the 5-min window would see their
      //    successful retry produce a contradictory "60 seconds expired"
      //    reply. setCooldown fires synchronously at handleSend entry,
      //    so sendCooldowns.has(userId) is true while a fresh send is
      //    in flight (entry is removed by clearCooldown on every exit
      //    path — the guard window is the in-flight duration only, NOT
      //    the full QURL_SEND_COOLDOWN_MS). The fresh flow's own
      //    collector handles the attachment in that case.
      //
      // Catcher is NOT armed on non-timeout errors (channel destroyed,
      // gateway disconnect, perms revoked). In those cases there was
      // never a working capture path, so a fresh awaitMessages on the
      // same channel would just fail again.
      //
      // The DM-type check is belt-and-suspenders: both branches that
      // assign captureChannel above today set a DM channel, so this is
      // effectively always true. The gate pins the invariant against a
      // future refactor that introduces a non-DM capture path (e.g. a
      // staff-only ephemeral capture in a guild channel).
      if (!isUnexpected && captureChannel.type === ChannelType.DM) {
        const senderUserId = interaction.user.id;
        const myGeneration = (lateDropGenerations.get(senderUserId) || 0) + 1;
        lateDropGenerations.set(senderUserId, myGeneration);
        captureChannel.awaitMessages({
          filter: (m) => m.author.id === senderUserId && m.attachments.size > 0,
          max: 1,
          time: 5 * 60000,
        }).then((late) => {
          // Stale-catcher bail: a newer late-drop catcher has been armed
          // (user timed out again). Only the newest catcher fires.
          if (lateDropGenerations.get(senderUserId) !== myGeneration) return;
          lateDropGenerations.delete(senderUserId);
          const lateMsg = late.first();
          if (!lateMsg) return;
          if (sendCooldowns.has(senderUserId)) return;
          lateMsg.reply('⏳ 60 seconds expired — please run `/qurl send` again to reattach.').catch((rErr) => logger.warn('Failed to send late-drop reattach hint', {
            sendNonce, userId: senderUserId, error: rErr?.message,
          }));
        }).catch((err) => {
          // awaitMessages without errors:['time'] resolves with an empty
          // Collection on timeout (handled above as !lateMsg) — anything
          // reaching this catch is an actual fetch / permission failure,
          // which means the late-drop feature is non-functional for this
          // user this session. logger.warn (not debug) so it surfaces.
          //
          // Mirror the .then() generation gate: under stacking, N stale
          // catchers can reject simultaneously on a real channel failure
          // (gateway disconnect drops them all). Without this gate, that
          // produces N warn lines for the same incident.
          if (lateDropGenerations.get(senderUserId) !== myGeneration) return;
          lateDropGenerations.delete(senderUserId);
          logger.warn('Late-drop awaitMessages rejected (non-timeout)', {
            sendNonce, userId: senderUserId, error: err?.message,
          });
        });
      }
      clearCooldown(interaction.user.id);
      return interaction.editReply({ content: 'No file received within 60 seconds. Send cancelled.', components: [] }).catch(logIgnoredDiscordErr);
    }

    attachment = fileMessage.attachments.first();

    // No fileMessage.delete() — bots can't delete user messages in
    // DMs, and the file's in a 1:1 thread anyway. We DO delete the
    // bot-authored prompt + "Got your file" confirmation for visual
    // cleanup; the user can clear their own attachment message.
    if (dmPromptMessage) {
      const cleanup = async () => {
        // Build a Link button back to the channel where /qurl send was
        // invoked. Discord URL format: discord.com/channels/<guild>/<channel>.
        // Clicking takes the user straight back to the channel where the
        // ephemeral form is waiting (ephemeral messages live for the
        // duration of the interaction token, ~15 min).
        const channelUrl = `https://discord.com/channels/${interaction.guild?.id ?? '@me'}/${interaction.channelId}`;
        const rawName = interaction.channel?.name;
        const channelLabel = rawName
          ? `Go back to #${rawName}`.slice(0, 80) // Discord button label cap is 80
          : 'Go back to the channel';
        const goBackRow = new ActionRowBuilder().addComponents(
          new ButtonBuilder()
            .setStyle(ButtonStyle.Link)
            .setLabel(channelLabel)
            .setURL(channelUrl)
        );

        let confirmMsg;
        try {
          confirmMsg = await captureChannel.send({
            content: '✓ **Got your file.** Click below to finish your send.',
            components: [goBackRow],
          });
        } catch (err) {
          logger.warn('Failed to post DM confirmation after file capture', {
            sendNonce, userId: interaction.user.id, error: err?.message,
          });
        }
        // Delete the original "Ready!" prompt right away — it's stale.
        dmPromptMessage.delete().catch((err) => logger.warn('Failed to delete DM prompt message', {
          sendNonce, userId: interaction.user.id, error: err?.message,
        }));
        // Delete the confirmation+button after 3 min — matches the
        // Step-3 form-loop timeout, so the navigation aid stays alive
        // for as long as the form itself is alive. setTimeout is fire-
        // and-forget; if the EC2 dies mid-flow the cleanup never fires
        // and the bot-side debris is low-impact (user can still delete
        // it manually).
        if (confirmMsg) {
          setTimeout(() => {
            confirmMsg.delete().catch((err) => logger.warn('Failed to delete DM confirmation', {
              sendNonce, userId: interaction.user.id, error: err?.message,
            }));
          }, 3 * 60000).unref?.();
        }
      };
      // Fire-and-forget but explicitly catch — guards against any future
      // edit that throws synchronously inside cleanup() and would
      // otherwise surface as UnhandledPromiseRejection.
      cleanup().catch((err) => logger.warn('DM cleanup failed', {
        sendNonce, userId: interaction.user.id, error: err?.message,
      }));
    }

    // Every error-path editReply below is wrapped with `.catch(logIgnoredDiscordErr)`
    // for parity with the timeout/cancel paths. Up to a few minutes can pass
    // between the user dropping the file and these guards firing on a slow form-loop, and
    // an `editReply` against a stale interaction can reject with "Unknown
    // Interaction" — swallow + log instead of surfacing as an unhandled rejection.
    if (!isAllowedFileType(attachment.contentType)) {
      clearCooldown(interaction.user.id);
      return interaction.editReply({
        content: `File type \`${attachment.contentType}\` is not allowed. Supported: images, PDFs, videos, audio, Office docs, text, CSV, ZIP.`,
        components: [],
      }).catch(logIgnoredDiscordErr);
    }
    if (attachment.size > MAX_FILE_SIZE) {
      clearCooldown(interaction.user.id);
      return interaction.editReply({
        content: `File too large (${Math.round(attachment.size / 1024 / 1024)}MB). Maximum is 25MB.`,
        components: [],
      }).catch(logIgnoredDiscordErr);
    }
    // Defense in depth — connector's downloadAndUpload validates this URL
    // again, but mirroring the same check inside handleAddRecipients keeps the SSRF
    // check at every callsite that hands an untrusted URL to the connector.
    // If discord.js ever changes how attachments report URL, the local
    // check fails fast with a clear message instead of relying on the
    // downstream module to remain in lockstep.
    if (!isAllowedSourceUrl(attachment.url)) {
      clearCooldown(interaction.user.id);
      logger.warn('File send rejected: attachment.url failed isAllowedSourceUrl', {
        sendNonce, userId: interaction.user.id, urlHost: safeUrlHost(attachment.url),
      });
      return interaction.editReply({
        content: 'The attached file URL is not from a recognized Discord CDN. Cancelled.',
        components: [],
      }).catch(logIgnoredDiscordErr);
    }
    // No early concurrency-cap check here. The original placement was
    // before the redesign added the up-to-3-minute Step-3 form loop;
    // by the time the user finishes the form, slot state has shifted
    // and the early "too busy" message is no longer informative — it
    // would also reject users who'd actually have a slot by Send-time.
    // The atomic re-check at the slot-claim site below (the
    // `if (activeFileSends >= MAX_CONCURRENT_FILE_SENDS)` guard inside
    // the back-half try-block) is the authoritative gate now and
    // produces the same user-facing message when the cap is genuinely
    // full at decision time.
  } else {
    resourceType = RESOURCE_TYPES.MAPS;

    const modal = new ModalBuilder()
      .setCustomId(`qurl_loc_modal_${sendNonce}`)
      .setTitle('Share a Location');

    const locationInput = new TextInputBuilder()
      .setCustomId('location_value')
      .setLabel('Google Maps link or place name')
      .setPlaceholder('https://maps.app.goo.gl/... or Eiffel Tower, Paris')
      .setStyle(TextInputStyle.Short)
      .setMaxLength(500)
      .setRequired(true);

    modal.addComponents(new ActionRowBuilder().addComponents(locationInput));
    // Fast-fail if showModal itself rejects (Unknown Interaction, expired
    // token, Discord 5xx). Without this, the handler would still proceed
    // to a 90-second awaitModalSubmit that can never resolve, surfacing as
    // a confusing "Location input timed out" message for what was really
    // an immediate API failure.
    let modalShown = true;
    try {
      await initBtn.showModal(modal);
    } catch (err) {
      modalShown = false;
      logger.warn('Location modal showModal rejected', {
        sendNonce, userId: interaction.user.id, error: err?.message,
      });
    }
    if (!modalShown) {
      clearCooldown(interaction.user.id);
      return interaction.editReply({
        content: 'Could not open the location input. Please try again.',
        components: [],
      }).catch(logIgnoredDiscordErr);
    }

    // Clear the underlying ephemeral message's components — without this,
    // the original 2-button reply (Send File / Send Location) stays
    // clickable while the modal is up. A click on Send File there has no
    // listener and Discord renders an "Interaction failed" toast. The
    // file path didn't have this issue because initBtn.update({components:
    // []}) clears the row before awaitMessages.
    await interaction.editReply({ components: [] }).catch(logIgnoredDiscordErr);

    let modalSubmit;
    try {
      modalSubmit = await initBtn.awaitModalSubmit({
        // Discord scopes modal-submit interactions to the user who saw
        // the modal, but defense-in-depth: match the personal-message
        // modal filter so a future reader can't spot a "missing user
        // check here" inconsistency.
        filter: (i) => i.customId === `qurl_loc_modal_${sendNonce}` && i.user.id === interaction.user.id,
        // 90s, not 120s. Trimmed to keep the location-path worst case
        // (60s init + 90s modal + 180s form = 5.5min) symmetric with the
        // file-path worst case (60s init + 60s awaitMessages + 180s form
        // = 5min) before the back-half starts. Both paths leave ~9-10
        // min of the 15-min interaction-token window for the back-half.
        time: 90000,
      });
    } catch (err) {
      const isTimeout = err?.code === 'InteractionCollectorError' || /time/.test(err?.message || '');
      if (!isTimeout) {
        logger.error('Modal submit failed unexpectedly', { sendNonce, error: err?.message });
      }
      clearCooldown(interaction.user.id);
      return interaction.editReply({
        content: isTimeout ? 'Location input timed out. Send cancelled.' : 'Could not collect location input. Send cancelled.',
        components: [],
      }).catch(logIgnoredDiscordErr);
    }

    // Hard-cap input length BEFORE regex matching to prevent ReDoS on
    // pathological strings against the unbounded character classes
    // inside MAPS_URL_PATTERNS. Real Google Maps URLs peak around ~300 chars.
    const locationValue = modalSubmit.fields.getTextInputValue('location_value').trim().slice(0, 2000);
    await modalSubmit.deferUpdate();

    ({ locationUrl, locationName } = parseLocationInput(locationValue));
    // Cap + escape markdown so a crafted place name can't inject
    // **bold** / links / code / spoilers into the recipient embed.
    if (locationName) locationName = escapeDiscordMarkdown(locationName.slice(0, 256));
  }

  // ── Step 3: Common final step ──
  // Single component message that gathers: target type → recipient
  // (UserSelect or auto-resolved channel/voice members) → optional
  // message (modal-driven button) → expiry (default 24h) → Send/Cancel.
  // Loop on component interactions until the user clicks Send or Cancel
  // (or the 3-min component timeout fires). State is held in closure
  // vars and re-rendered via `compInt.update(...)` on each change.
  let target = null;
  let recipients = [];
  let personalMessage = null;
  let expiresIn = '24h';
  // Self-destruct timer — one of SELF_DESTRUCT_PRESETS in seconds, or null
  // for no timer (default). Set via the form's StringSelectMenu;
  // forwarded to the connector as `viewer_ttl_seconds` at upload time.
  let selfDestructSeconds = null;

  const formId = `qurl_form_${sendNonce}`;
  // Component customIds for the form-loop filter. Every entry must be a
  // top-level component the loop's awaitMessageComponent can dispatch on
  // (button, string select, user select). Modal customIds are NOT here —
  // they live as local consts inside their own handler so the form-loop
  // filter set stays tight.
  const ids = {
    targetSelect: `${formId}_target`,
    userSelect: `${formId}_user`,
    selfDestructSelect: `${formId}_destruct_select`,
    expirySelect: `${formId}_expiry`,
    messageBtn: `${formId}_msg_btn`,
    sendBtn: `${formId}_send`,
    cancelBtn: `${formId}_cancel`,
  };
  // Modal customIds — local to their respective button handlers; never
  // consumed by the form-loop filter, kept out of `ids` so
  // Object.values(ids) doesn't grow noise.
  const messageModalId = `${formId}_msg_modal`;

  // Sender's own filename in their own ephemeral form preview is low-blast,
  // but match the same defense the location path applies to locationName so
  // a future copy-paste of formContent() into a non-ephemeral surface
  // doesn't quietly leak a markdown-injection vector. Computed once per
  // send (the attachment doesn't change after Step 2) so formContent()
  // doesn't re-escape on every form re-render.
  const safeAttachmentName = attachment ? escapeDiscordMarkdown(attachment.name) : '';

  // `warning` is an optional reason string prepended with the ⚠️ glyph
  // INSTEAD of the default ✅ prefix. Caller-side string concatenation
  // would rendered "⚠️ ... \n\n✅ Got X" — visually mixed signals — so the
  // helper owns the leading glyph and switches on the warning shape.
  const formContent = ({ warning } = {}) => {
    let content = warning ? `⚠\u{FE0F} ${warning}\n\n` : '✅ ';
    if (resourceType === RESOURCE_TYPES.FILE) content += `Got **${safeAttachmentName}**. You can delete the file in DM after send.`;
    else content += `Got **${locationName || 'location'}**.`;
    content += '\n\nChoose recipient(s), optionally add a message, pick expiry, then **Send**.';
    if (personalMessage) {
      const preview = personalMessage.length > 80 ? personalMessage.slice(0, 80) + '…' : personalMessage;
      content += `\n\n_Note:_ "${preview}"`;
    }
    // Same isFinite + > 0 invariant the dropdown's `default` flag uses
    // (see `hasTimer` in formRows below) — a corrupted DDB row carrying
    // Infinity is truthy and would otherwise surface as
    // "_Self-destruct timer:_ (invalid)" in the form preview.
    if (Number.isFinite(selfDestructSeconds) && selfDestructSeconds > 0) {
      content += `\n\n_Self-destruct timer:_ ${formatSelfDestructLabel(selfDestructSeconds)}`;
    }
    return content;
  };

  // Contextual label — voice/stage-voice paths resolve to voice-connected
  // only (see `getChannelMembers`'s doc-comment in src/discord.js for
  // the v14 polymorphism + the bug this avoids).
  const channelOptionLabel = isVoiceContext
    ? 'Everyone in this voice channel'
    : 'Everyone in this channel';
  const channelOptionDescription = isVoiceContext
    ? 'Members currently connected via voice (excl. bots and you)'
    : 'All members of this text channel (excl. bots and you)';

  const formRows = () => {
    const rows = [];

    rows.push(new ActionRowBuilder().addComponents(
      new StringSelectMenuBuilder()
        .setCustomId(ids.targetSelect)
        .setPlaceholder('Recipient(s) — choose type')
        .addOptions(
          { label: 'A specific user', value: 'user', description: 'Pick one or more users to send to', default: target === 'user' },
          { label: channelOptionLabel, value: 'channel', description: channelOptionDescription, default: target === 'channel' },
        )
    ));

    if (target === 'user') {
      rows.push(new ActionRowBuilder().addComponents(
        new UserSelectMenuBuilder()
          .setCustomId(ids.userSelect)
          .setPlaceholder('Pick one or more users')
          .setMinValues(1)
          .setMaxValues(Math.min(USER_SELECT_PER_PICK_CAP, config.QURL_SEND_MAX_RECIPIENTS))
      ));
    }

    // Self-destruct timer dropdown — true select-from-list rather than a
    // modal interstitial. Discord StringSelectMenus need their own
    // ActionRow (can't combine with buttons), so the messageBtn moves
    // into the send/cancel row below to keep the form within the 5-row
    // max when target=user.
    //
    // The "default" flag must match the current state so re-renders show
    // the user's pick as the select's collapsed-header text. Same
    // isFinite + > 0 invariant the form preview uses — a corrupted DDB
    // row carrying Infinity is truthy and would otherwise mark a wrong
    // option default.
    // hasTimer = current state has a positive-finite seconds value.
    // hasMatchingPreset = that value matches one of the 7 presets.
    // The two diverge only for off-preset finite values (only reachable
    // today via a hypothetical backfilled sendConfig.self_destruct_seconds
    // outside the preset set). Defaulting the no-timer option on
    // `!hasMatchingPreset` keeps the dropdown header sensible — without
    // it, every option is un-defaulted and Discord falls back to
    // rendering the first option's label in the collapsed header,
    // which would conflict with the formContent preview line that
    // echoes the off-preset value via formatSelfDestructLabel.
    const hasTimer = Number.isFinite(selfDestructSeconds) && selfDestructSeconds > 0;
    const hasMatchingPreset = hasTimer && SELF_DESTRUCT_PRESETS.some((p) => p.seconds === selfDestructSeconds);
    // No setPlaceholder — the No-timer option ships default-true when
    // !hasTimer, so the dropdown header always reflects the current
    // state and a placeholder would never render. Same default-true
    // convention expirySelect uses for its current-value option.
    rows.push(new ActionRowBuilder().addComponents(
      new StringSelectMenuBuilder()
        .setCustomId(ids.selfDestructSelect)
        // Explicit min/max=1 (Discord's default, matches userSelect's
        // explicit settings on this form). The empty-values defense in
        // the handler stays as belt-and-suspenders for forged payloads.
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

    rows.push(new ActionRowBuilder().addComponents(
      new StringSelectMenuBuilder()
        .setCustomId(ids.expirySelect)
        .setPlaceholder('Link expiry')
        .addOptions(
          ...EXPIRY_CHOICES.map(c => ({ label: c.name, value: c.value, default: c.value === expiresIn }))
        )
    ));

    const recipientsResolved = (target === 'user' && recipients.length >= 1)
      || (target === 'channel' && recipients.length > 0);
    // Bottom row packs the optional-note button + send + cancel. Discord
    // allows up to 5 buttons per ActionRow; 3 fits comfortably. Left-
    // to-right reading order puts the optional affordance before the
    // commit action, which matches how the rest of the form is laid out
    // (target → user → optional bits → submit).
    rows.push(new ActionRowBuilder().addComponents(
      new ButtonBuilder()
        .setCustomId(ids.messageBtn)
        .setLabel(personalMessage ? '✏\u{FE0F} Edit note' : '✏\u{FE0F} Add a note (optional)')
        .setStyle(ButtonStyle.Primary),
      new ButtonBuilder()
        .setCustomId(ids.sendBtn)
        .setLabel('\u{1F4E4} Send')
        .setStyle(ButtonStyle.Success)
        .setDisabled(!recipientsResolved),
      new ButtonBuilder()
        .setCustomId(ids.cancelBtn)
        .setLabel('Cancel')
        .setStyle(ButtonStyle.Secondary)
    ));

    return rows;
  };

  await interaction.editReply({ content: formContent(), components: formRows() }).catch(logIgnoredDiscordErr);

  // Pre-build the customId set once; the form-loop filter runs on every
  // component event for the next 3 min and Object.values(ids) would
  // otherwise rebuild the array on each dispatch.
  const formCompIdSet = new Set(Object.values(ids));

  // Wrap every component-update call in the form loop with the same
  // .catch(logIgnoredDiscordErr) the front-half error-path editReplys
  // use. The compInt is freshly issued by awaitMessageComponent so a
  // rejection is rare in practice (a user closing Discord between the
  // click landing and the update going out is the realistic case),
  // but mirroring the editReply convention keeps the unhandled-
  // rejection surface uniform across handleSend.
  const safeCompUpdate = (ci, payload) => ci.update(payload).catch(logIgnoredDiscordErr);
  const safeCompDefer = (ci) => ci.deferUpdate().catch(logIgnoredDiscordErr);

  let sendApproved = false;
  while (!sendApproved) {
    let compInt;
    try {
      compInt = await interaction.channel.awaitMessageComponent({
        filter: (i) => i.user.id === interaction.user.id && formCompIdSet.has(i.customId),
        // 3 min, not 5: total time-since-original-`reply()` must stay under
        // Discord's 15-min interaction-token expiry, including 60s init
        // wait + 60s file awaitMessages + back-half (download → re-upload
        // → mint batches → DM fan-out). 5 min on the form alone left only
        // ~9 min for the back-half, which a 25 MB file with batched
        // re-uploads on a slow upstream can blow through. 3 min keeps the
        // back-half headroom comfortable while still being plenty of
        // recipient-picking time for any realistic flow.
        time: 3 * 60000,
      });
    } catch {
      clearCooldown(interaction.user.id);
      return interaction.editReply({ content: 'Send timed out (3 min). Cancelled.', components: [] }).catch(logIgnoredDiscordErr);
    }

    if (compInt.customId === ids.cancelBtn) {
      clearCooldown(interaction.user.id);
      await safeCompUpdate(compInt, { content: 'Send cancelled.', components: [] });
      return;
    }

    if (compInt.customId === ids.sendBtn) {
      await safeCompUpdate(compInt, { content: 'Preparing send…', components: [] });
      sendApproved = true;
      break;
    }

    if (compInt.customId === ids.targetSelect) {
      const newTarget = compInt.values[0];
      // For user-target, re-selecting 'user' is a no-op (UserSelect drives
      // the actual recipient pick). For channel, ALWAYS re-resolve.
      // Staleness sources differ by context:
      //   - text channel: members can join/leave the guild during the
      //     up-to-3-min form-loop window; the guild member cache (warmed
      //     by fetchGuildMembers below) goes stale.
      //   - voice / stage-voice: users can join/leave voice during the
      //     window; the gateway-driven voice state cache reflects this
      //     live, but a captured recipients array snapshot would not.
      // A stale recipients array would mint links for ghosts.
      if (newTarget !== target || newTarget === 'channel') {
        target = newTarget;
        recipients = [];
        if (target === 'channel') {
          // Voice / stage-voice channels source members from the voice
          // state cache (populated automatically via gateway events) — no
          // guild.members.fetch needed. Text channels need the cache warm.
          if (!isVoiceContext) {
            try {
              await fetchGuildMembers(interaction.guild);
            } catch (err) {
              logger.error('Failed to fetch guild members', { error: err.message, sendNonce, userId: interaction.user.id, guildId: interaction.guild?.id });
              clearCooldown(interaction.user.id);
              await safeCompUpdate(compInt, { content: 'Failed to load channel members. Send cancelled.', components: [] });
              return;
            }
          }
          recipients = getChannelMembers(interaction.channel, interaction.user.id);
          if (recipients.length === 0) {
            target = null;
            const warning = isVoiceContext
              ? 'No other users currently connected to this voice channel. Pick **A specific user** instead.'
              : 'No other members in this channel. Pick **A specific user** instead.';
            await safeCompUpdate(compInt, { content: formContent({ warning }), components: formRows() });
            continue;
          }
        }
        // Surface the per-send cap NOW, while the user can change targets,
        // not 3 minutes later after they've filled in the rest of the form.
        // The post-Send check below stays as a final defense; this is a UX
        // fast-fail so the user isn't sandbagged.
        if (recipients.length > config.QURL_SEND_MAX_RECIPIENTS) {
          const resolvedCount = recipients.length;
          target = null;
          recipients = [];
          await safeCompUpdate(compInt, {
            content: formContent({ warning: `This ${newTarget} has ${resolvedCount} members — over the per-send cap of ${config.QURL_SEND_MAX_RECIPIENTS}. Pick a different target or split into multiple \`/qurl send\` runs.` }),
            components: formRows(),
          });
          continue;
        }
      }
      await safeCompUpdate(compInt, { content: formContent(), components: formRows() });
      continue;
    }

    if (compInt.customId === ids.userSelect) {
      // REPLACE semantic (not append): Discord's user-select shows
      // the previously-picked set as the default and the user edits
      // it; un-picking Bob and adding Carol must yield [Carol], not
      // [Bob, Carol]. Use the channel-target's "Add more recipients"
      // button for the additive path.
      const selected = [...compInt.users.values()];
      if (selected.length === 0) {
        await safeCompDefer(compInt);
        continue;
      }
      // Fail-loud over silent-drop — operator re-picks without the
      // offender. Matches the prior single-pick UX.
      if (selected.some(u => u.bot)) {
        await safeCompUpdate(compInt, { content: formContent({ warning: 'Cannot send to a bot. Re-pick without any bot users.' }), components: formRows() });
        continue;
      }
      if (selected.some(u => u.id === interaction.user.id)) {
        await safeCompUpdate(compInt, { content: formContent({ warning: 'Cannot send to yourself. Re-pick without your own user.' }), components: formRows() });
        continue;
      }
      recipients = selected;
      await safeCompUpdate(compInt, { content: formContent(), components: formRows() });
      continue;
    }

    if (compInt.customId === ids.messageBtn) {
      // Local-only — modal text inputs are read by customId inside this
      // handler, never dispatched through the form-loop filter. Keeping
      // it out of `ids` avoids polluting Object.values(ids) lookups.
      const messageInputId = 'message_value';
      const msgModal = new ModalBuilder()
        .setCustomId(messageModalId)
        .setTitle('Personal message');
      msgModal.addComponents(new ActionRowBuilder().addComponents(
        new TextInputBuilder()
          .setCustomId(messageInputId)
          .setLabel('Optional note (leave blank to clear)')
          .setStyle(TextInputStyle.Paragraph)
          .setMaxLength(280)
          .setRequired(false)
          .setValue(personalMessage || '')
      ));
      await compInt.showModal(msgModal).catch(logIgnoredDiscordErr);

      let msgSubmit;
      try {
        msgSubmit = await compInt.awaitModalSubmit({
          filter: (i) => i.customId === messageModalId && i.user.id === interaction.user.id,
          time: 120000,
        });
      } catch {
        // Intentional silent no-op on modal timeout: the form state is
        // unchanged (personalMessage holds whatever it was before the
        // user opened the modal), so re-rendering would just redraw the
        // same form. We deliberately skip a "modal closed" toast here
        // because it would also fire on the legitimate close-without-
        // typing case, which is normal user behavior.
        continue;
      }

      const raw = msgSubmit.fields.getTextInputValue(messageInputId).trim();
      personalMessage = raw ? sanitizeMessage(raw) : null;
      await msgSubmit.update({ content: formContent(), components: formRows() }).catch(logIgnoredDiscordErr);
      continue;
    }

    if (compInt.customId === ids.selfDestructSelect) {
      // Single-pick StringSelectMenu — value is one of the option values
      // we set on the form: the no-timer sentinel or `String(preset.seconds)`.
      // selfDestructSelectValueToSeconds owns the conversion and falls
      // back to null (no timer) for any unexpected value (forged
      // interaction / option-list drift), which is the safe default.
      // Optional chaining is defense-in-depth — discord.js normalizes
      // the gateway payload to `values: []` for String selects, so
      // missing `values` is only reachable via a constructed-by-hand
      // compInt (test path or forged event). The `?.` is still cheap.
      selfDestructSeconds = selfDestructSelectValueToSeconds(compInt.values?.[0]);
      await safeCompUpdate(compInt, { content: formContent(), components: formRows() });
      continue;
    }

    if (compInt.customId === ids.expirySelect) {
      expiresIn = compInt.values[0];
      await safeCompUpdate(compInt, { content: formContent(), components: formRows() });
      continue;
    }
  }

  // Defense in depth: the Send button is `setDisabled` until recipients
  // resolve, but a spoofed component interaction could still fire it with
  // an empty recipient set. Bail early so the back-half doesn't mint zero
  // links and surface a confusing "Failed to create any links" message.
  if (recipients.length === 0) {
    clearCooldown(interaction.user.id);
    return interaction.editReply({
      content: 'No recipients selected. Send cancelled.',
      components: [],
    });
  }

  if (recipients.length > config.QURL_SEND_MAX_RECIPIENTS) {
    clearCooldown(interaction.user.id);
    const overBy = recipients.length - config.QURL_SEND_MAX_RECIPIENTS;
    return interaction.editReply({
      content: `This send targets ${recipients.length} recipients, but the per-send cap is ${config.QURL_SEND_MAX_RECIPIENTS}. Trim ${overBy} recipient${overBy === 1 ? '' : 's'} from the channel/group, or split into multiple \`/qurl send\` runs.`,
      components: [],
    });
  }

  // --- Step 4: Process and send (back-half — extracted to executeSendPipeline). ---
  // `return await` (not bare `await`) so the docstring's "Resolved value"
  // contract matches the call shape. `return await` keeps the async
  // frame on the stack until the awaited promise settles, which
  // preserves the executeSendPipeline frame in rejection traces —
  // `return promise` (no await) would detach it, costing forensic
  // signal. There's a ~1-microtask perf cost vs the no-await form;
  // negligible at this scale, worth the better traces.
  return await executeSendPipeline(interaction, {
    apiKey,
    resourceType,
    attachment,
    locationUrl,
    locationName,
    recipients,
    target,
    isVoiceContext,
    expiresIn,
    selfDestructSeconds,
    personalMessage,
    sendNonce,
  });
}

// Handle adding new recipients to an existing send. senderDiscordId is
// derived from originalInteraction directly so no caller can pass a
// mismatched value and accidentally let one user add recipients to another
// user's send.
async function handleAddRecipients(sendId, usersCollection, originalInteraction, apiKey) {
  const senderDiscordId = originalInteraction.user.id;
  const sendConfig = await db.getSendConfig(sendId, senderDiscordId);
  if (!sendConfig) {
    return { msg: 'Send configuration not found.', newResourceIds: [], delivered: 0, failed: 0, newRecipients: [] };
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
    return { msg: 'No valid recipients selected (bots and yourself are excluded).', newResourceIds: [], delivered: 0, failed: 0, newRecipients: resolvedRecipients };
  }

  // Create new QURL links for each resource type in the send config
  // recipientLinks[recipientId] = [{ qurlLink, resourceId, resType, label }]
  const recipientLinks = {};
  const hasFile = sendConfig.connector_resource_id;
  const hasLocation = sendConfig.actual_url;

  if (!hasFile && !hasLocation) {
    return { msg: 'Cannot add recipients — send configuration is incomplete.', newResourceIds: [], delivered: 0, failed: 0, newRecipients: resolvedRecipients };
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
  try {
    if (hasFile) {
      // Re-download from the stored Discord CDN URL, then upload a fresh
      // resource so the 10-token pool is full. Re-upload again every
      // TOKENS_PER_RESOURCE recipients. The original resource is drained by
      // the initial send, so we CANNOT reuse sendConfig.connector_resource_id.
      if (!sendConfig.attachment_url) {
        return {
          msg: 'Cannot add file recipients — original attachment is no longer available. Please create a new send.',
          newResourceIds: [], newRecipients: resolvedRecipients,
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
          newResourceIds: [], newRecipients: resolvedRecipients,
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
        });
      } catch (err) {
        // Discord CDN URLs are signed and expire (~24h). If re-download fails,
        // surface a clear user-facing message; log the real error server-side
        // so err.message (which may echo upstream response detail) never
        // reaches a Discord reply.
        const isExpired = /403|expired|network|CDN/i.test(err.message || '');
        const msg = isExpired
          ? 'Original attachment URL has expired. Please create a new send.'
          : 'Failed to prepare links. Please try again, or create a new send if the issue persists.';
        logger.error('addRecipients file re-upload failed', { sendId, error: err.message, isExpired });
        return { msg, newResourceIds: [], delivered: 0, failed: 0, newRecipients: resolvedRecipients };
      }

      if (allLinks.length < newRecipients.length) {
        logger.error('mintLinks returned fewer links than expected in addRecipients', { expected: newRecipients.length, got: allLinks.length });
        const newResourceIds = [...new Set(allLinks.map(l => l.resourceId))];
        return { msg: `Only ${allLinks.length} of ${newRecipients.length} links created. Try again.`, newResourceIds, delivered: 0, failed: 0, newRecipients: resolvedRecipients };
      }
      // Iterate by allLinks length so an off-by-one can never index out of bounds.
      // The guard above ensures allLinks.length >= newRecipients.length.
      for (let i = 0; i < allLinks.length; i++) {
        const r = newRecipients[i];
        if (!r) break;
        if (!recipientLinks[r.id]) recipientLinks[r.id] = [];
        recipientLinks[r.id].push({
          qurlLink: allLinks[i].qurl_link,
          resourceId: allLinks[i].resourceId,
          resType: RESOURCE_TYPES.FILE,
          label: `File (${sanitizeFilename(sendConfig.attachment_name || 'file')})`,
        });
      }
      preparedKinds.push('file');
    }
    if (hasLocation) {
      const locPayload = { type: 'google-map', url: sendConfig.actual_url, name: sendConfig.location_name || 'Google Maps Location' };
      const firstUpload = await uploadJsonToConnector(locPayload, 'location.json', apiKey, inheritedDestruct);
      const expiresAt = expiryToISO(sendConfig.expires_in);
      const allLinks = await mintLinksInBatches({
        initialResourceId: firstUpload.resource_id,
        reuploadFn: () => uploadJsonToConnector(locPayload, 'location.json', apiKey, inheritedDestruct),
        expiresAt,
        recipientCount: newRecipients.length,
        apiKey,
      });

      if (allLinks.length < newRecipients.length) {
        logger.error('mintLinks returned fewer links than expected in addRecipients (location)', { expected: newRecipients.length, got: allLinks.length });
        return { msg: `Only ${allLinks.length} of ${newRecipients.length} location links created. Try again.`, newResourceIds: [], delivered: 0, failed: 0, newRecipients: resolvedRecipients };
      }
      newRecipients.forEach((r, i) => {
        if (!recipientLinks[r.id]) recipientLinks[r.id] = [];
        recipientLinks[r.id].push({
          qurlLink: allLinks[i].qurl_link,
          resourceId: allLinks[i].resourceId,
          resType: RESOURCE_TYPES.MAPS, label: sendConfig.location_name || 'Google Maps',
        });
      });
      preparedKinds.push('location');
    }
  } catch (error) {
    logger.error('Failed to create links for additional recipients', { error: error.message });
    const isPoolExhausted = error.message?.includes('429') || error.message?.includes('limit');
    const msg = isPoolExhausted
      ? 'Link pool exhausted for this resource. Please create a new send instead of adding recipients.'
      : 'Failed to create links for new recipients.';
    return { msg, newResourceIds: [], delivered: 0, failed: 0, newRecipients: resolvedRecipients };
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
    return { msg: 'Failed to create any links.', newResourceIds: [], delivered: 0, failed: 0, newRecipients: resolvedRecipients };
  }

  // Persist to DB before DMs
  const batchSends = [];
  for (const [rid, links] of Object.entries(recipientLinks)) {
    for (const link of links) {
      batchSends.push({
        sendId, senderDiscordId, recipientDiscordId: rid, resourceId: link.resourceId,
        resourceType: link.resType, qurlLink: link.qurlLink, expiresIn: sendConfig.expires_in,
        channelId: originalInteraction.channelId, targetType: 'user',
      });
    }
  }
  // Same guarantee as handleSend: if the DB write fails, abort BEFORE any
  // DMs go out so we don't leave live QURL links with no local record.
  try {
    await db.recordQURLSendBatch(batchSends);
  } catch (err) {
    logger.error('recordQURLSendBatch failed in addRecipients; aborting before DMs', {
      sendId, error: err.message, linkCount: batchSends.length,
    });
    return {
      msg: 'Failed to save link records. Recipients were not messaged. Please try again.',
      newResourceIds: [], newRecipients: resolvedRecipients,
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
    // try/finally + before-DB-write — see handleSend's batchSettled
    // callback for the full rationale (payload-build, sendDM-throws,
    // AND DB-throw must all still emit the metric). Wraps the entire
    // dispatch attempt — payload assembly, button re-packing, network
    // call — so a malformed sendConfig (e.g. pathological
    // personalMessage that throws inside buildDeliveryPayload) still
    // counts as dispatch_failed instead of disappearing from CloudWatch.
    let sent = false;
    try {
      // Same Unix-seconds expiry computation as handleSend — see comment
      // there. Computed once per recipient (cheap; the alternative would
      // be threading a single timestamp through sendConfig).
      const expiresAt = Math.floor((Date.now() + expiryToMs(sendConfig.expires_in)) / 1000);
      const payloads = links.slice(0, 10).map(link => buildDeliveryPayload({
        // Same alias resolution as handleSend — see comment there for
        // the nickname > globalName > username fallback rationale.
        senderAlias: resolveSenderAlias(originalInteraction),
        qurlLink: link.qurlLink,
        expiresAt,
        personalMessage: sendConfig.personal_message,
      }));
      // Contract with buildDeliveryPayload: each payload's `components`
      // array contains exactly one ActionRow whose `components` are the
      // per-link buttons. We pull each payload's first row's children
      // (one Step Through button per link) and re-pack into 5-per-row
      // ActionRows since Discord caps at 5 buttons per ActionRow.
      const allEmbeds = payloads.flatMap(p => p.embeds);
      const allButtons = payloads.flatMap(p => {
        // Hard fail rather than silently drop buttons if the contract
        // ever changes — easier to catch than a button quietly missing
        // from a recipient's DM.
        if (!p.components || !p.components[0] || !Array.isArray(p.components[0].components)) {
          throw new Error('buildDeliveryPayload contract violated: expected components[0].components to be an array');
        }
        return p.components[0].components;
      });
      const allComponents = [];
      for (let i = 0; i < allButtons.length; i += 5) {
        allComponents.push(new ActionRowBuilder().addComponents(allButtons.slice(i, i + 5)));
      }

      sent = await sendDM(recipient.id, { embeds: allEmbeds, components: allComponents });
    } finally {
      logger.audit(sent === true ? AUDIT_EVENTS.DISPATCH_SENT : AUDIT_EVENTS.DISPATCH_FAILED, { send_id: sendId });
    }
    // updateSendDMStatus updates every qurl_sends row matching (sendId,
    // recipient.id), so a single call covers all links for this recipient.
    // The previous `for (let i = 0; i < links.length; i++)` loop wrote the
    // same update links.length times.
    await db.updateSendDMStatus(sendId, recipient.id, sent ? DM_STATUS.SENT : DM_STATUS.FAILED);
    return { sent, username: recipient.username };
  }, 5);

  for (const r of dmResults) {
    if (r.status === 'fulfilled' && r.value.sent) delivered++;
    else failed++;
  }

  const allLinks = Object.values(recipientLinks).flat();
  const newResourceIds = [...new Set(allLinks.map(l => l.resourceId))];
  let msg = `Added ${delivered} recipient${delivered !== 1 ? 's' : ''}`;
  if (failed > 0) msg += ` (${failed} could not be reached)`;
  logger.info('/qurl add recipients', { sendId, delivered, failed });
  // Return delivered/failed explicitly so callers don't have to regex-parse msg.
  return { msg, newResourceIds, delivered, failed, newRecipients: resolvedRecipients };
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
  const revoked = await revokeAllLinks(sendId, interaction.user.id, apiKey);

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
// backed — its persistence is the signed state token + pending_links
// SQLite table, and there's no `await*` in process to convert.
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
  + 'Your team can use `/qurl send` to share files and locations securely.\n'
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
// /qurl file + /qurl map — slash-driven sends.
//
// Two new subcommands replace `/qurl send`'s button-driven wizard
// with all-options-up-front slash commands. The flow:
//
//   /qurl file recipients:<text> attachment:<file> [expires-in]
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
//
// /qurl send stays in place until PR 7b.3 hard-removes it.
// ─────────────────────────────────────────────────────────────

const {
  parseRecipientMentions,
  MAX_SLASH_OPTION_LENGTH: RECIPIENTS_SLASH_MAX_LENGTH,
} = require('./recipient-parser');

const SEND_STAGE_AWAITING_CONFIRM = 'awaiting_send_confirm';

// CustomIds — PREFIX-ONLY (no nonce). flow-dispatch's loadFlow already
// gates routing on stage + version; encoding identity into customId
// would not add safety (the dispatcher's trust model treats customId
// as a routing key, not an identity signal). Matches the convention
// REVOKE_SELECT_CUSTOM_ID / SETUP_BUTTON_CUSTOM_ID use.
const SEND_USER_SELECT_CUSTOM_ID = 'qurl_send_user_select';
const SEND_CONFIRM_SEND_CUSTOM_ID = 'qurl_send_confirm_send';
const SEND_CONFIRM_CANCEL_CUSTOM_ID = 'qurl_send_confirm_cancel';

// 3-minute confirm-card window. Matches /qurl send's Step-3 form
// timeout at line ~2293 so users have the same time-to-finish budget
// whichever entry point they use.
const SEND_FLOW_TTL_SECONDS = 180;

// Slash-option choice arrays. Reuse the existing /qurl send wording
// so users see the same labels in autocomplete as in the form's
// dropdowns. EXPIRY_CHOICES already exists. SELF_DESTRUCT_CHOICES is
// new here because /qurl send wires self-destruct via a StringSelect
// post-invocation rather than a slash option.
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
  if (!Number.isFinite(n) || n <= 0) return null;
  const floored = Math.floor(n);
  if (!SELF_DESTRUCT_PRESET_SECONDS.has(floored)) return null;
  return floored;
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
// server" misdirection cr round 4 flagged.
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

// Filter resolved Users: drop bots, drop the sender. Returns
// `{ valid, droppedBots, droppedSelf }` so the caller can surface
// "X bots / your-own-id were dropped" as a distinct error from
// "no recipients at all".
//
// Known v1 cap-skew: parseRecipientMentions caps to QURL_SEND_MAX_RECIPIENTS
// BEFORE knowing which IDs are bots. A mention list of
// [bot1, bot2, ..., bot25, user1] with cap=25 yields ids=[bot1..bot25]
// and post-filter valid=[]. The user sees "all recipients dropped" and
// has to re-run with bots removed. Acceptable since (a) it requires a
// pathological mention list to trip, (b) the failure mode is loud
// rather than silent. A future refactor can resolve-then-cap if this
// becomes a real complaint.
function partitionRecipients(users, senderId) {
  const valid = [];
  let droppedBots = 0;
  let droppedSelf = 0;
  for (const u of users) {
    if (u.bot) { droppedBots++; continue; }
    if (u.id === senderId) { droppedSelf++; continue; }
    valid.push(u);
  }
  return { valid, droppedBots, droppedSelf };
}

// Returns the empty string when nothing is worth surfacing — keeps
// callers simple (`warningsBlock + content`) without a separate
// "do I have any warnings" check.
function renderRecipientWarnings({
  invalidTokens = [],
  cappedCount = 0,
  unresolvedIds = [],
  transientFailureIds = [],
  droppedBots = 0,
  droppedSelf = 0,
} = {}) {
  const lines = [];
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
    const shown = invalidTokens.slice(0, 10).map((t) => t.replace(/`/g, ''));
    const more = invalidTokens.length > 10 ? ` (+${invalidTokens.length - 10} more)` : '';
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
  if (droppedSelf > 0) {
    lines.push('• You cannot send a qURL to yourself — skipped.');
  }
  if (lines.length === 0) return '';
  return '⚠\u{FE0F} **Some recipients were dropped:**\n' + lines.join('\n') + '\n\n';
}

// Build the confirm-card content string. Mirrors /qurl send's Step-3
// content shape (formContent at ~line 2130) so users see consistent
// wording across both entry points. Brand spelling is `qURL` per
// CLAUDE.md — the user-visible copy here, the slash-command
// descriptions, and any logger.audit user-facing fields all preserve
// the case.
function renderConfirmCardContent({
  resourceType, resourceLabel, validRecipients,
  expiresIn, selfDestructSeconds, personalMessage,
  warningsBlock, needsPicker,
}) {
  let content = warningsBlock || '';
  if (resourceType === RESOURCE_TYPES.FILE) {
    content += `📁 **Sending file:** ${resourceLabel}\n`;
  } else {
    content += `🗺\u{FE0F} **Sending location:** ${resourceLabel}\n`;
  }
  if (needsPicker) {
    content += '\n**Pick recipients below** (1–'
      + String(Math.min(USER_SELECT_PER_PICK_CAP, config.QURL_SEND_MAX_RECIPIENTS))
      + ' users), then click **Send**.\n';
  } else {
    // First-N preview keeps the card scannable when a paste resolves
    // to many users. Discord-name display via resolveRecipientAlias
    // matches the post-send confirmation wording.
    const PREVIEW = 5;
    const previewNames = validRecipients.slice(0, PREVIEW)
      .map((u) => escapeDiscordMarkdown(u.username))
      .join(', ');
    const more = validRecipients.length > PREVIEW ? `, +${validRecipients.length - PREVIEW} more` : '';
    content += `\n**To:** ${validRecipients.length} user${validRecipients.length === 1 ? '' : 's'} (${previewNames}${more})\n`;
  }
  content += `**Expires:** ${EXPIRY_LABELS[expiresIn] || expiresIn}\n`;
  if (Number.isFinite(selfDestructSeconds) && selfDestructSeconds > 0) {
    content += `**Self-destruct:** ${formatSelfDestructLabel(selfDestructSeconds)}\n`;
  }
  if (personalMessage) {
    // `personalMessage` is pre-sanitized: sanitizeMessage already
    // ran markdown-escape + @-mention strip at the slash-option
    // boundary. Re-escaping here would render `\*\*bold\*\*`
    // literals on the card. Same shape /qurl send's Step-3
    // formContent uses (commands.js:~2164).
    //
    // Quote-block syntax `> ` (instead of `"..."` wraps) avoids the
    // ragged-look failure mode when the message itself contains a
    // literal `"` character. Discord renders `> ` as a left-bar
    // blockquote which visually offsets the preview from the rest
    // of the card.
    content += `**Note:** ${formatPersonalMessagePreview(personalMessage)}\n`;
  }
  content += '\nClick **Send** to deliver one-time qURL links, or **Cancel** to abort.';
  return content;
}

// Truncate the pre-sanitized message to 80 chars MAX, then back off
// to the last completed markdown-escape so a slice doesn't leave a
// lone trailing `\`. The blockquote prefix (`> `) is rendered by
// Discord as a left-bar offset.
//
// The "back off" handles the case where `sanitizeMessage` emitted a
// `\` immediately before the slice boundary (e.g. `\*` becomes `\`
// at index 79, `*` at index 80 — slicing at 80 leaves a dangling
// `\` that Discord would render as a literal backslash). Trimming
// to index 79 in that case is the conservative fix.
function formatPersonalMessagePreview(message) {
  if (message.length <= 80) return `> ${message}`;
  let cut = 80;
  if (message[cut - 1] === '\\' && message[cut - 2] !== '\\') {
    // The would-be cut sits ON an escape char. Back off so the
    // truncation lands BEFORE the escape, not in the middle of it.
    cut -= 1;
  }
  return `> ${message.slice(0, cut)}…`;
}

// Build the ActionRow set for the confirm card. When `attachPicker`,
// row 0 is a UserSelectMenu (recipients absent OR a prior pick is in
// the payload and the user might want to re-pick); otherwise no
// picker row. Send/Cancel always in the last row.
//
// Renamed from `needsPicker` so the content/rows divergence at the
// post-pick call site (content rendered WITHOUT picker, rows rendered
// WITH picker — to let the user re-pick) is impossible to silently
// merge in a future refactor.
function renderConfirmCardRows({ attachPicker, sendDisabled }) {
  const rows = [];
  if (attachPicker) {
    const maxValues = Math.min(USER_SELECT_PER_PICK_CAP, config.QURL_SEND_MAX_RECIPIENTS);
    rows.push(new ActionRowBuilder().addComponents(
      new UserSelectMenuBuilder()
        .setCustomId(SEND_USER_SELECT_CUSTOM_ID)
        .setPlaceholder(`Pick recipients (1–${maxValues})`)
        .setMinValues(1)
        .setMaxValues(maxValues)
    ));
  }
  rows.push(new ActionRowBuilder().addComponents(
    new ButtonBuilder()
      .setCustomId(SEND_CONFIRM_SEND_CUSTOM_ID)
      .setLabel('\u{1F4E4} Send')
      .setStyle(ButtonStyle.Success)
      .setDisabled(sendDisabled),
    new ButtonBuilder()
      .setCustomId(SEND_CONFIRM_CANCEL_CUSTOM_ID)
      .setLabel('Cancel')
      .setStyle(ButtonStyle.Secondary),
  ));
  return rows;
}

// Shared entry point — both `/qurl file` and `/qurl map` route here
// after collecting their resource-specific options. `params` carries:
//   resourceType: RESOURCE_TYPES.FILE | RESOURCE_TYPES.MAPS
//   attachment:   {url, name, contentType, size} | null  (file path)
//   locationUrl:  string | null                          (map path)
//   locationName: string | null                          (map path)
//   resourceLabel: string                                (rendered on card)
// apiKey is INTENTIONALLY not threaded through the front-half.
// handleSendConfirmClick re-fetches the guild API key at Send time
// — the key may rotate during the confirm card's 3-min TTL, and the
// dispatcher's gate at the slash-command entry point only proves the
// key was present at that single moment. Re-fetching at click time
// is the durable check. The dispatcher's gate (SEND_LIKE_SUBCOMMANDS
// set) still runs to fail fast on the no-key-at-all case.
async function handleQurlSlashSend(interaction, params) {
  if (!interaction.guildId || !interaction.guild) {
    return interaction.reply({
      content: 'This command can only be used in a server, not in DMs.',
      ephemeral: true,
    });
  }
  if (isOnCooldown(interaction.user.id)) {
    return interaction.reply({
      content: 'Please wait before sending again.',
      ephemeral: true,
    });
  }
  setCooldown(interaction.user.id);

  // Top-level try/catch fences unanticipated throws (a TypeError from
  // a malformed cached member, a future parser change, etc.) from
  // leaving the user stranded in a cooldown window for a failure
  // that never produced a visible response. Every known failure
  // mode below has its own targeted clearCooldown + ephemeral
  // editReply; this catch is the safety net for everything else.
  try {
    await interaction.deferReply({ ephemeral: true });

    const recipientsRaw = interaction.options.getString('recipients');
    const expiresIn = interaction.options.getString('expires-in') || '24h';
    const selfDestructValue = interaction.options.getString('self-destruct') || SELF_DESTRUCT_NO_TIMER_CHOICE;
    const personalMessageRaw = interaction.options.getString('personal-message');

    // Defense-in-depth: expiresIn comes from the slash command's choice
    // list which Discord enforces server-side, but a forged interaction
    // could carry an off-set value. EXPIRY_LABELS owns the closed set.
    if (!Object.prototype.hasOwnProperty.call(EXPIRY_LABELS, expiresIn)) {
      clearCooldown(interaction.user.id);
      return interaction.editReply({
        content: '❌ Unrecognized expiry value. Re-run and pick from the list.',
      });
    }
    const selfDestructSeconds = selfDestructOptionToSeconds(selfDestructValue);
    const personalMessage = personalMessageRaw ? sanitizeMessage(personalMessageRaw) : null;

    const parsed = parseRecipientMentions(recipientsRaw, interaction);

    let resolved = { users: [], unresolvedIds: [], transientFailureIds: [] };
    if (parsed.ids.length > 0) {
      try {
        resolved = await resolveRecipientUsers(interaction, parsed.ids);
      } catch (err) {
        clearCooldown(interaction.user.id);
        logger.error('qurl file/map: resolveRecipientUsers threw', {
          user_id: interaction.user.id, error: err && err.message,
        });
        return interaction.editReply({
          content: '❌ Could not resolve recipients. Please try again.',
        });
      }
    }

    const { valid, droppedBots, droppedSelf } = partitionRecipients(resolved.users, interaction.user.id);

    // `needsPicker` is true when the user supplied no `recipients:`
    // value at all. When they DID supply one but it post-filtered to
    // empty, hard-fail with a specific error so the user knows why
    // (vs. silently dropping into the picker, which would mask the
    // underlying mention-list problem).
    const recipientsOmitted = recipientsRaw == null || recipientsRaw.trim().length === 0;
    const warningsBlock = renderRecipientWarnings({
      invalidTokens: parsed.invalidTokens,
      cappedCount: parsed.cappedCount,
      unresolvedIds: resolved.unresolvedIds,
      transientFailureIds: resolved.transientFailureIds,
      droppedBots,
      droppedSelf,
    });
    if (!recipientsOmitted && valid.length === 0) {
      clearCooldown(interaction.user.id);
      // Parser silently strips bots + the sender from `<@id>` mentions
      // when the cache reports them (recipient-parser.js:205, 214) —
      // those IDs never reach `partitionRecipients`. If post-parse `ids`
      // is empty AND the user supplied non-empty `recipients`, surface
      // a "nothing usable" message even when partition's breakdown is
      // zero. This is what bot-only / self-only mention lists hit.
      const breakdownEmpty = droppedBots === 0 && droppedSelf === 0
        && resolved.unresolvedIds.length === 0
        && resolved.transientFailureIds.length === 0
        && parsed.invalidTokens.length === 0
        && parsed.cappedCount === 0;
      const detail = breakdownEmpty
        ? '\n\nMake sure you @-mention real users (bots and your own user are skipped automatically).'
        : '';
      return interaction.editReply({
        content: warningsBlock + '❌ **No valid recipients to send to.** Re-run with at least one valid user mention.' + detail,
      });
    }

    const needsPicker = recipientsOmitted;
    const recipientIds = valid.map((u) => u.id);

    // supersedeOrCreate handles the "another /qurl file/map is open"
    // case — sibling-flow disambig surfaces a stage-specific message;
    // same-stage rerun atomically claims the slot. Mirrors /qurl
    // revoke's pattern.
    const flow_id = flowIdForInteraction(interaction);
    const sendNonce = crypto.randomBytes(8).toString('hex');
    const payload = {
      resourceType: params.resourceType,
      attachment: params.attachment,
      locationUrl: params.locationUrl,
      locationName: params.locationName,
      resourceLabel: params.resourceLabel,
      recipientIds,
      expiresIn,
      selfDestructSeconds,
      personalMessage,
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

    const content = renderConfirmCardContent({
      resourceType: params.resourceType,
      resourceLabel: params.resourceLabel,
      validRecipients: valid,
      expiresIn,
      selfDestructSeconds,
      personalMessage,
      warningsBlock,
      needsPicker,
    });
    const rows = renderConfirmCardRows({
      attachPicker: needsPicker,
      sendDisabled: needsPicker,  // Send stays disabled until UserSelectMenu fires
    });
    return interaction.editReply({ content, components: rows });
  } catch (err) {
    // Unanticipated throw. Always clear cooldown — the user got no
    // visible response, so they must not be locked out for the full
    // cooldown window. Try to surface a generic error via the
    // (already-deferred) reply; if the editReply ALSO throws (token
    // expired, Discord blip), the safety-net catch in flow-dispatch's
    // handleFlowInteraction handles the front-half error visibility.
    clearCooldown(interaction.user.id);
    logger.error('handleQurlSlashSend: unexpected throw', {
      user_id: interaction.user.id, error: err && err.message, stack: err && err.stack,
    });
    return interaction.editReply({
      content: '❌ Something went wrong — please try again.',
    }).catch(logIgnoredDiscordErr);
  }
}

async function handleQurlFile(interaction) {
  // UX fast-fail. The real concurrency-slot claim happens inside
  // executeSendPipeline (commands.js:~1043); this pre-check is best-
  // effort — by Send-click time the cap state can have shifted, in
  // which case the back-half's guard fires instead.
  if (activeFileSends >= MAX_CONCURRENT_FILE_SENDS) {
    return interaction.reply({
      content: 'The bot is processing too many file sends right now. Please try again in a moment.',
      ephemeral: true,
    });
  }

  // Required-option lookups via `getAttachment(..., true)` /
  // `getString(..., true)` throw on a missing option. Discord enforces
  // required server-side, so production interactions can't trip
  // these; a forged interaction is the only way. The targeted catch
  // produces an actionable ephemeral rather than letting the throw
  // hit the dispatcher's generic safety net.
  let attachment;
  try {
    attachment = interaction.options.getAttachment('attachment', true);
  } catch (err) {
    logger.warn('handleQurlFile: required attachment option missing', {
      user_id: interaction.user.id, error: err && err.message,
    });
    return interaction.reply({
      content: '❌ The `attachment:` option is required. Re-run with a file attached.',
      ephemeral: true,
    });
  }
  if (!attachment || typeof attachment.url !== 'string') {
    return interaction.reply({
      content: '❌ Attachment is missing or malformed.',
      ephemeral: true,
    });
  }
  if (!isAllowedSourceUrl(attachment.url)) {
    logger.warn('handleQurlFile: attachment.url failed SSRF gate', {
      user_id: interaction.user.id, host: safeUrlHost(attachment.url),
    });
    return interaction.reply({
      content: '❌ Attachment source not allowed. Files must be uploaded via Discord, not linked from external URLs.',
      ephemeral: true,
    });
  }
  if (!isAllowedFileType(attachment.contentType)) {
    return interaction.reply({
      content: `❌ File type not allowed: \`${escapeDiscordMarkdown(String(attachment.contentType || 'unknown'))}\`.`,
      ephemeral: true,
    });
  }
  if (attachment.size > MAX_FILE_SIZE) {
    // Match /qurl send's MB formatting at commands.js:~1994 so users
    // see the same readable cap across all three entry points.
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
    // 256-char cap mirrors the map-path `locationName` cap so a future
    // upload-pipeline change that loosens Discord's filename ceiling
    // can't bloat the confirm card. String() coerces in case a
    // hypothetical future caller hands a non-string name.
    resourceLabel: escapeDiscordMarkdown(String(attachment.name).slice(0, 256)),
  });
}

async function handleQurlMap(interaction) {
  // `getString('location', true)` throws on a missing option. Discord
  // enforces required server-side; only a forged interaction can hit
  // this. Targeted catch matches handleQurlFile's required-option
  // guard for symmetry + actionable user-visible copy.
  let locationValue;
  try {
    locationValue = interaction.options.getString('location', true).trim();
  } catch (err) {
    logger.warn('handleQurlMap: required location option missing', {
      user_id: interaction.user.id, error: err && err.message,
    });
    return interaction.reply({
      content: '❌ The `location:` option is required. Re-run with a Google Maps URL or address.',
      ephemeral: true,
    });
  }
  const locationNameRaw = interaction.options.getString('location-name');
  if (locationValue.length === 0) {
    return interaction.reply({
      content: '❌ Location is empty.',
      ephemeral: true,
    });
  }

  // Shared parser: see `parseLocationInput` near the top of this file.
  // `/qurl send`'s modal-driven location text calls it too — keep both
  // entry points in lockstep so they produce identical delivered URLs
  // for the same input.
  let { locationUrl, locationName } = parseLocationInput(locationValue);
  // Explicit location-name override wins over the URL-derived name.
  if (locationNameRaw && locationNameRaw.trim().length > 0) {
    locationName = locationNameRaw.trim();
  }
  if (locationName) locationName = escapeDiscordMarkdown(locationName.slice(0, 256));

  return handleQurlSlashSend(interaction, {
    resourceType: RESOURCE_TYPES.MAPS,
    attachment: null,
    locationUrl,
    locationName,
    resourceLabel: locationName || 'location',
  });
}

// Stage stays at SEND_STAGE_AWAITING_CONFIRM — `transitionFlow` with
// `stage_to` === current stage advances the version (OCC guard) and
// refreshes the TTL, so repeated picker churn doesn't expire the row
// while the user is still deciding.
async function handleSendUserSelect(interaction, { flow_id, row }) {
  const selected = [...interaction.users.values()];
  if (selected.length === 0) {
    return interaction.deferUpdate().catch(logIgnoredDiscordErr);
  }
  // No `resolveRecipientUsers` re-fetch here — Discord's
  // UserSelectMenu only surfaces users visible to the bot in this
  // guild, so picked User IDs are guild-bounded at the gateway-event
  // level. handleSendConfirmClick re-fetches at click time
  // (partial-drop test pins this) as the actual guild-membership
  // defense; adding it here would burn 10 members.fetch calls per
  // picker tick without catching anything the Send-time check misses.
  const { valid, droppedBots, droppedSelf } = partitionRecipients(selected, interaction.user.id);
  if (valid.length === 0) {
    return interaction.update({
      content: '⚠\u{FE0F} ' + (droppedBots > 0 ? 'Cannot send to bots.' : 'Cannot send to yourself.')
        + ' Re-pick recipients below.',
      components: renderConfirmCardRows({ attachPicker: true, sendDisabled: true }),
    }).catch(logIgnoredDiscordErr);
  }
  // Defense-in-depth — unreachable in production today: the picker's
  // setMaxValues caps at min(USER_SELECT_PER_PICK_CAP=10,
  // QURL_SEND_MAX_RECIPIENTS=25) = 10, so the user physically can't
  // pick more than 25. Kept against a future bump to either constant
  // (or a forged interaction) so the cap stays honored.
  if (valid.length > config.QURL_SEND_MAX_RECIPIENTS) {
    return interaction.update({
      content: `⚠\u{FE0F} Pick at most ${config.QURL_SEND_MAX_RECIPIENTS} recipients.`,
      components: renderConfirmCardRows({ attachPicker: true, sendDisabled: true }),
    }).catch(logIgnoredDiscordErr);
  }

  const payload = row.payload || {};
  const newPayload = { ...payload, recipientIds: valid.map((u) => u.id) };
  const result = await transitionFlow(flow_id, row.version, {
    stage_to: SEND_STAGE_AWAITING_CONFIRM,
    payload: newPayload,
    terminal: false,
    set_expires_at: Math.floor(Date.now() / 1000) + SEND_FLOW_TTL_SECONDS,
  });
  if (result.result === 'conflict') {
    return interaction.update({
      content: 'Send was superseded — re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }
  if (result.result === 'not_found') {
    return interaction.update({
      content: 'This send expired — re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }

  // Recompute warnings against the new pick. `droppedBots` AND
  // `droppedSelf` can flip here — Discord's UserSelectMenu doesn't
  // exclude the invoker, so a user picking themselves bumps
  // droppedSelf; bots present in the picker bump droppedBots.
  // `cappedCount` / `invalidTokens` / unresolved / transient buckets
  // are string-path-only (parser never ran on the picker selection),
  // so they stay empty here. The earlier zero-valid short-circuit
  // already handled the empty case; what's left below is the
  // partial-pick UX where SOME picked users are valid but the
  // warning still surfaces the dropped ones.
  const warningsBlock = renderRecipientWarnings({
    droppedBots,
    droppedSelf,
  });
  // Intentional flag divergence: the CONTENT renders the resolved
  // recipient summary ("**To:** N user(s)") — needsPicker:false —
  // while the ROWS keep the UserSelectMenu attached so the user can
  // re-pick. Send is enabled because we now have a valid recipient
  // set. The two flags are deliberately fed opposite values; the
  // rename from `needsPicker` to `attachPicker` on the rows side
  // makes this asymmetry impossible to silently consolidate.
  const content = renderConfirmCardContent({
    resourceType: payload.resourceType,
    resourceLabel: payload.resourceLabel,
    validRecipients: valid,
    expiresIn: payload.expiresIn,
    selfDestructSeconds: payload.selfDestructSeconds,
    personalMessage: payload.personalMessage,
    warningsBlock,
    needsPicker: false,
  });
  return interaction.update({
    content,
    components: renderConfirmCardRows({ attachPicker: true, sendDisabled: false }),
  }).catch(logIgnoredDiscordErr);
}

// Send button → fire executeSendPipeline. deleteFlow first as the
// dedup primitive (duplicate dispatch under future SQS at-least-once
// must not double-send). Mirrors handleRevokeSelect's ordering.
async function handleSendConfirmClick(interaction, { flow_id, row }) {
  const payload = row.payload || {};
  // Re-fetch users at click time — defensive against members leaving
  // the guild between confirm and Send. Both unresolved buckets
  // (10007 + transient fetch failure) reduce the delivered count,
  // so both are surfaced to the sender via followUp below — without
  // the split, a 429 / gateway blip would silently shrink the
  // delivered set with no signal.
  //
  // resolveRecipientUsers is wrapped in try/catch because the dispatcher's
  // outer catch only logs + replies generic-superseded; a click-time
  // lookup throw should give the user a directly actionable message
  // without committing the dedup deleteFlow.
  let resolved;
  try {
    resolved = await resolveRecipientUsers(interaction, payload.recipientIds || []);
  } catch (err) {
    logger.error('handleSendConfirmClick: resolveRecipientUsers threw', {
      flow_id, error: err && err.message,
    });
    // `interaction.reply` (not safeReply / editReply) is intentional —
    // this is a button-click dispatched by flow-dispatch, the
    // interaction is in the unacked state at this point. A refactor
    // that swaps in deferReply earlier in the handler would need to
    // flip this to editReply too.
    return interaction.reply({
      content: '❌ Could not look up recipients right now. Try **Send** again in a moment.',
      ephemeral: true,
    }).catch(logIgnoredDiscordErr);
  }
  const { users, unresolvedIds, transientFailureIds } = resolved;
  const partialLeftCount = unresolvedIds.length;
  const partialTransientCount = transientFailureIds.length;
  if (partialLeftCount > 0 || partialTransientCount > 0) {
    logger.info('handleSendConfirmClick: partial drop at click time', {
      flow_id, left: partialLeftCount, transient: partialTransientCount,
    });
  }
  const { valid } = partitionRecipients(users, interaction.user.id);
  if (valid.length === 0) {
    // Every recipient resolved at confirm time has since left the
    // guild or become unreachable. Terminal for THIS attempt —
    // delete the flow so a rerun claims a fresh slot.
    await deleteFlow(flow_id, {
      stage: SEND_STAGE_AWAITING_CONFIRM,
      reason: 'terminal',
    }).catch((err) => logger.warn('handleSendConfirmClick: deleteFlow on empty failed', {
      flow_id, error: err && err.message,
    }));
    return interaction.update({
      content: '❌ Recipients are no longer reachable (all left the server). Re-run the command.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }

  // Resolve apiKey before dedup — getGuildApiKey is idempotent + free,
  // so a duplicate dispatch's redundant call is harmless. Same
  // safety rule handleRevokeSelect documents (parallelize ONLY with
  // idempotent reads).
  //
  // `expectedVersion: row.version` fences the picker-then-Send race:
  // if a UserSelectMenu interaction landed and transitioned the flow
  // between the dispatcher's loadFlow (which fed `row` here) and our
  // deleteFlow, the version would have advanced. Without the version
  // gate, we'd deleteFlow successfully and call executeSendPipeline
  // with the STALE recipientIds captured in `payload` above — exactly
  // the silent-divergence shape handleSetupButton uses expectedVersion
  // to fence (commands.js:~3196). `deleted: false` here now collapses
  // duplicate dispatch AND mid-flight picker mutation; both map to the
  // same user recovery ("the card moved under you, re-click Send").
  // `interaction.guildId` is guaranteed non-null at this point —
  // handleQurlSlashSend rejects DM invocations BEFORE the flow row
  // is created, so a row at SEND_STAGE_AWAITING_CONFIRM only ever
  // belongs to a guild interaction. No conditional fallback.
  //
  // Sequence: getGuildApiKey BEFORE deleteFlow. If the DDB read
  // throws (rare — region outage, IAM revocation), the row stays
  // alive and the user can re-click Send within the TTL once the
  // blip clears. Burning the flow row first would strand the user
  // on a dead card with the dispatcher's generic-superseded message
  // and force a full /qurl file rerun.
  let guildApiKey;
  try {
    guildApiKey = await db.getGuildApiKey(interaction.guildId);
  } catch (err) {
    logger.error('handleSendConfirmClick: getGuildApiKey threw', {
      flow_id, error: err && err.message,
    });
    return interaction.reply({
      content: '❌ Could not look up the qURL API key right now. Try **Send** again in a moment.',
      ephemeral: true,
    }).catch(logIgnoredDiscordErr);
  }
  const deleteResult = await deleteFlow(flow_id, {
    stage: SEND_STAGE_AWAITING_CONFIRM,
    reason: 'terminal',
    expectedVersion: row.version,
  });
  if (!deleteResult.deleted) {
    return interaction.reply({
      content: 'Recipients changed before Send fired — re-check the card and click Send again.',
      ephemeral: true,
    }).catch(logIgnoredDiscordErr);
  }

  const apiKey = guildApiKey || config.QURL_API_KEY;
  if (!apiKey) {
    return interaction.update({
      content: '❌ qURL is no longer configured for this server. Ask an admin to run `/qurl setup`.',
      components: [],
    }).catch(logIgnoredDiscordErr);
  }

  // Ack with a "Preparing send…" placeholder; executeSendPipeline
  // takes over the editReply from here. Matches handleSend's hand-off
  // shape at commands.js:~2307.
  await interaction.update({ content: 'Preparing send…', components: [] }).catch(logIgnoredDiscordErr);

  // Surface partial drops (members who left the guild between confirm
  // and Send, OR who failed lookup transiently) as a separate
  // ephemeral followUp BEFORE the back-half takes over the main
  // reply. Without this the user would see "Sent to N users" with no
  // signal that N != the count shown on the card. Distinct wording
  // for the two buckets — "left the server" is stable, "lookup
  // blipped" encourages a fresh /qurl file rerun if they want to
  // include the missed recipients.
  if (partialLeftCount > 0 || partialTransientCount > 0) {
    const parts = [];
    if (partialLeftCount > 0) {
      parts.push(`${partialLeftCount} recipient${partialLeftCount === 1 ? '' : 's'} had left the server`);
    }
    if (partialTransientCount > 0) {
      parts.push(`${partialTransientCount} couldn't be looked up just now (rerun /qurl file to retry them)`);
    }
    await interaction.followUp({
      content: `ℹ\u{FE0F} ${parts.join('; ')} — sending to the remaining ${valid.length}.`,
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
    target: 'user',
    isVoiceContext: false,
    expiresIn: payload.expiresIn,
    selfDestructSeconds: payload.selfDestructSeconds,
    personalMessage: payload.personalMessage,
    sendNonce: payload.sendNonce,
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
// `clearCooldown` is gated on `deleted: true` so the cooldown stays
// honored on the Cancel-loser branch.
async function handleSendCancelClick(interaction, { flow_id, row }) {
  const { deleted } = await deleteFlow(flow_id, {
    stage: SEND_STAGE_AWAITING_CONFIRM,
    reason: 'terminal',
    expectedVersion: row.version,
  });
  if (!deleted) {
    return interaction.reply({
      content: 'This send was already processed or the card moved — re-check it.',
      ephemeral: true,
    }).catch(logIgnoredDiscordErr);
  }
  clearCooldown(interaction.user.id);
  return interaction.update({
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
// attachment and drop the Show All button (the file IS the full list).
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
// ActionRowBuilder/ButtonBuilder for the Show All / Show Less toggle.
// All wording assertions live against `renderRevokeContent` directly
// (see `apps/discord/src/revoke-render.js` + the e2e smoke).
function renderRevokeMsg(sendId, names, total, showAll, success = names.length) {
  const data = renderRevokeContent({ names, total, showAll, success });
  const row = data.needsExpand
    ? new ActionRowBuilder().addComponents(
      new ButtonBuilder()
        .setCustomId(`qurl_revoke_expand_${sendId}`)
        .setLabel(showAll ? 'Show Less' : 'Show All')
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
  delivered, expiresIn,
  failedNamesPlain = [], successNames = [], showAll = false,
}) {
  const header = `Sent to ${delivered} user${delivered !== 1 ? 's' : ''} | Expires: ${expiresIn} | One-time links`;
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

async function revokeAllLinks(sendId, senderDiscordId, apiKey) {
  // Pull per-recipient items so per-link success can be mapped back to
  // a Discord user id for display. Caller resolves IDs → usernames
  // against its in-scope `recipients` array.
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

  const totalUsers = new Set(items.map(it => it.recipient_discord_id)).size;
  const success = successUserIds.length;
  const total = totalUsers;
  // Audit metric is per-resource (DELETE call), not per-recipient.
  const auditTotal = byResource.size;
  const auditSuccess = results.filter(r => r.status === 'fulfilled').length;

  // Record the user's revocation intent so this send stops appearing in
  // the /qurl revoke dropdown. Mark regardless of per-link success —
  // partial failures surface in the reply ("Revoked X/Y"), and re-
  // picking the same send wouldn't help anyway. Emit audit BEFORE
  // markSendRevoked so a DB write throw can't suppress the metric.
  if (total > 0) {
    const event = success > 0 ? AUDIT_EVENTS.REVOKE_SUCCESS : AUDIT_EVENTS.REVOKE_FAILED;
    logger.audit(event, { send_id: sendId, success: auditSuccess, total: auditTotal });
  }
  await db.markSendRevoked(sendId, senderDiscordId);

  // Top-level `success/total` are per-resource (matches the audit
  // event); per-recipient counts surface in nested `users`.
  logger.info('Revoked send', {
    sendId,
    success: auditSuccess,
    total: auditTotal,
    users: { success, total },
  });
  // failureUserIds is computed but not yet rendered — the "Note:
  // already-opened links cannot be revoked" disclaimer covers the
  // common cause. Returned for callers that want to surface partial-
  // failure detail (e.g., a future "Failed for: …" line or follow-up
  // alert when count is large).
  return { success, total, successUserIds, failureUserIds };
}

// Time-based sweep every 60s (was 5min). With high user counts the Map can
// creep between the size-based 10k-threshold eviction, so a more aggressive
// proactive sweep keeps steady-state memory tight. Entries older than the
// cooldown window are safe to drop — they'd pass isOnCooldown() anyway.
setInterval(() => {
  const now = Date.now();
  for (const [k, v] of sendCooldowns) {
    if (now - v > config.QURL_SEND_COOLDOWN_MS) sendCooldowns.delete(k);
  }
}, 60 * 1000).unref();

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

        // Get all members with Contributor role
        const members = await guild.members.fetch();
        const contributors = members.filter(m => m.roles.cache.has(contributorRole.id));

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
    data: new SlashCommandBuilder()
      .setName('qurl')
      .setDescription('Share resources securely via qURL')
      .addSubcommand(sub =>
        sub.setName('send')
          .setDescription('Send a file or location to users via one-time secure link')
          // No options — the redesigned flow is button-driven. Customer
          // feedback (April 2026): the prior 4-option shape (target,
          // expiry_optional, file_optional, message_optional) confused
          // users at the slash-command-popup stage because they had to
          // pre-decide file-vs-location AND fill in target before
          // seeing the actual flow. New flow: `/qurl send` opens a
          // 2-button reply (Send File / Send Location); each path
          // collects its specific resource (drop-file in chat for
          // file via awaitMessages, modal text input for location)
          // then converges on a shared final step that gathers
          // recipient(s), optional message, and expiry before
          // committing the send.
          //
          // PR 7b.3 hard-removes /qurl send in favor of /qurl file
          // and /qurl map. The two new subcommands take all options
          // up-front so power users can stay on the keyboard, and
          // the in-channel confirm card is flow_state-backed.
      )
      .addSubcommand(sub =>
        sub.setName('file')
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
              .setMaxLength(280)
          )
      )
      .addSubcommand(sub =>
        sub.setName('map')
          .setDescription('Share a Google Maps location via one-time qURL links')
          .addStringOption(opt =>
            opt.setName('location')
              .setDescription('Google Maps URL, or a place / address to search')
              .setRequired(true)
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
              .setMaxLength(280)
          )
      )
      .addSubcommand(sub =>
        sub.setName('revoke')
          .setDescription('Revoke links from a previous send')
      )
      .addSubcommand(sub =>
        sub.setName('help')
          .setDescription('Show qURL bot help')
      )
      .addSubcommand(sub =>
        sub.setName('setup')
          .setDescription('Configure your qURL API key for this server (admin only)')
      )
      .addSubcommand(sub =>
        sub.setName('status')
          .setDescription('Check if qURL is configured (admin only)')
      ),
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

      // Gate: require guild API key for send/file/map/revoke. The
      // `Set.has` lookup keeps the gate's allowlist single-source —
      // adding a new send-style subcommand in 7b.3+ only requires
      // touching this set, not the dispatch fall-through below.
      const SEND_LIKE_SUBCOMMANDS = new Set(['send', 'file', 'map', 'revoke']);
      let resolvedApiKey = null;
      if (SEND_LIKE_SUBCOMMANDS.has(sub)) {
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

      // Pass API key as an explicit parameter rather than monkey-patching
      // the discord.js interaction object — that pattern is fragile (libs
      // sometimes clone/serialize interactions) and a security smell for
      // secret-bearing values.
      if (sub === 'send') return handleSend(interaction, resolvedApiKey);
      // /qurl file and /qurl map deliberately don't accept the
      // dispatcher-resolved apiKey — handleSendConfirmClick re-fetches
      // at Send time so a mid-flow rotation still uses the live key.
      // The dispatcher's SEND_LIKE_SUBCOMMANDS gate above is the
      // fail-fast presence check.
      if (sub === 'file') return handleQurlFile(interaction);
      if (sub === 'map') return handleQurlMap(interaction);
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
        return interaction.reply({
          content: '**qURL Bot — Help**\n\n' +
            '**Getting started — Share resources securely via one-time links:**\n' +
            '  `/qurl file` — share a file with users via one-time qURL links\n' +
            '  `/qurl map` — share a Google Maps location via one-time qURL links\n' +
            '  `/qurl send` — legacy button-driven flow (use `/qurl file` or `/qurl map` instead)\n' +
            '  `/qurl revoke` — revoke links from a previous send\n' +
            '  `/qurl help` — show this message\n\n' +
            '**How it works:**\n' +
            // Leading tab on "1." keeps Discord's markdown parser from
            // treating it as the start of an ordered list (which would
            // renumber it relative to the subsequent lines and visually
            // misalign "1." with "2.", "3.", "4."). The tab indent now
            // matches the two-space indent below, but bypasses the list
            // auto-formatter.
            '\t1. Run `/qurl file` (attach a file) or `/qurl map` (paste a Google Maps URL or address)\n' +
            '  2. Optionally `recipients:@a @b @role` (up to 25 users via @mentions or role expansion) — '
              + 'leave blank to pick from a menu (up to 10 at a time)\n' +
            '  3. Confirm the card, then click **Send**\n' +
            '  4. Recipients get a one-time link by DM that self-destructs on first access (or when the expiry elapses)\n\n' +
            oauthSetupSection +
            '**Terms:** a *protected resource* is the file or location you\'re sharing. ' +
            'A *qurl* (or *access link*) is the single-use URL that delivers it. ' +
            'You create a qurl for a protected resource each time you run `/qurl file` or `/qurl map`.\n\n' +
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
// /qurl send install like the test playground or a customer server) gets
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
// not purge each other on a fresh .set() call, so a bot that previously
// ran in OpenNHP mode (GUILD_ID=X, full command set registered to guild
// X) and is now redeployed in multi-tenant or single-guild-plain mode
// will leave /link, /leaderboard, etc. visible in X's slash-command
// autocomplete until Discord's cache ages out. The dispatch-time filter
// in handleCommand prevents those stale commands from doing anything
// harmful, but users still see dead commands in the picker. Issuing
// .set([], guildId) clears the guild-scoped set; any ALIVE guild-scoped
// registration for the CURRENT mode gets reinstalled by the main
// registerCommands path below.
//
// Scoped to non-OpenNHP modes: in OpenNHP mode we intentionally register
// guild-scoped commands to config.GUILD_ID, so purging there would
// race with the upcoming set(). Only iterates guilds the bot is
// actually in (client.guilds.cache) — we can't and shouldn't enumerate
// guilds we've never joined.
async function purgeStaleGuildCommands(client) {
  if (config.isOpenNHPActive) return; // guild-scoped register is the goal in OpenNHP mode
  // Parallelize the per-guild fetch+set. Sequentializing makes boot
  // time O(guilds) at ~500ms per round-trip, which scales badly for
  // the public-bot install path this PR targets. Promise.allSettled
  // so one slow/failing guild doesn't block the others, and so a
  // single rejection doesn't bubble and abort registerCommands.
  // Discord's guild-commands endpoint has a separate rate bucket
  // per guild, so parallel fans out cleanly until the global
  // app-command rate limit (~200/min) — well above any realistic
  // boot-time burst.
  const guilds = [...client.guilds.cache.values()];
  await Promise.allSettled(guilds.map(async (guild) => {
    try {
      const existing = await client.application.commands.fetch({ guildId: guild.id });
      if (existing.size === 0) return;
      await client.application.commands.set([], guild.id);
      logger.info(`Purged ${existing.size} stale guild-scoped commands from ${guild.name} (${guild.id})`);
    } catch (error) {
      // Don't fail boot on a purge error — the dispatch-time filter in
      // handleCommand is the correctness guarantee; purge is UX polish.
      logger.warn(`Could not purge stale commands from guild ${guild.id}`, { error: error.message });
    }
  }));
}

// Register commands with Discord. `config.isOpenNHPActive` is the
// single source of truth for "this deployment exercises the OpenNHP
// community surface" — see config.js for the derivation.
async function registerCommands(client) {
  // Purge first — prevents stale OpenNHP-era registrations in guilds the
  // bot is still in. See purgeStaleGuildCommands for details + why this
  // is scoped to non-OpenNHP modes.
  await purgeStaleGuildCommands(client);

  const activeCommands = getActiveCommands();
  const commandData = activeCommands.map(cmd => cmd.data.toJSON());

  try {
    if (config.GUILD_ID) {
      // Guild-scoped registration: commands appear instantly in just this
      // guild. Used by the single-guild OpenNHP deployment where fast command
      // iteration matters more than appearing in other guilds.
      logger.info(`Registering ${activeCommands.length} slash commands to guild ${config.GUILD_ID}...`);
      await client.application.commands.set(commandData, config.GUILD_ID);
    } else {
      // Global registration: commands appear in every guild the bot joins.
      // Discord caches global commands for up to 1 hour, so newly-added
      // commands may take that long to propagate. Used for multi-tenant
      // deployments (customers invite the bot to their own servers).
      logger.info(`Registering ${activeCommands.length} slash commands globally (multi-tenant mode): ${activeCommands.map(c => c.data.name).join(', ')}`);
      await client.application.commands.set(commandData);
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

// Handle command interactions
async function handleCommand(interaction) {
  // /qurl now has zero options after the button-driven redesign; no other
  // command in this file uses autocomplete. Discord can still deliver
  // autocomplete events (legacy clients, stale registrations) — short-
  // circuit them so they don't fall through to the chat-input path.
  if (interaction.isAutocomplete()) return;

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

// /qurl file + /qurl map confirm-card components. All three customIds
// share the same expectedStage — they're three component types
// (button + user-select + button) attached to the same confirm-card
// message, all routed by stage. siblingMessage is registered on the
// FIRST customId only; flow-dispatch rejects mismatched re-registrations
// of the same stage so we don't repeat it.
registerFlow(SEND_USER_SELECT_CUSTOM_ID, {
  expectedStage: SEND_STAGE_AWAITING_CONFIRM,
  handler: handleSendUserSelect,
  siblingMessage: 'You have a `/qurl file` or `/qurl map` confirm card open in this channel — finish or cancel it first.',
});
registerFlow(SEND_CONFIRM_SEND_CUSTOM_ID, {
  expectedStage: SEND_STAGE_AWAITING_CONFIRM,
  handler: handleSendConfirmClick,
});
registerFlow(SEND_CONFIRM_CANCEL_CUSTOM_ID, {
  expectedStage: SEND_STAGE_AWAITING_CONFIRM,
  handler: handleSendCancelClick,
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
  handleSendUserSelect,
  handleSendConfirmClick,
  handleSendCancelClick,
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
      batchSettled,
      expiryToISO,
      sendCooldowns,
      lateDropGenerations,
      handleAddRecipients,
      buildDeliveryPayload,
      resolveSenderAlias,
      safeUrlHost,
      // Back-half functions exposed for direct unit testing. Without these
      // hooks, coverage of the polling/revoke/add-recipients code paths
      // can only be reached via full handleSend integration tests, which
      // require mocking the entire state-machine front-half before the
      // back-half even runs. Direct exposure means each function gets a
      // focused spec without that setup overhead.
      monitorLinkStatus,
      revokeAllLinks,
      renderRevokeMsg,
      renderSendConfirm,
      REVOKE_TRUNC_LIMIT,
      mintLinksInBatches,
      activeMonitors,
      // The top-level back-half driver. Exported here so PR 7b's
      // tests (and the follow-up direct unit spec in #278) can pin
      // its contract against a constructed param object — without
      // re-driving handleSend's Step-1-through-Step-3 wizard or the
      // future flow_state-backed form-fill.
      executeSendPipeline,
      // Test-only file-concurrency hooks. The slot counter is module-
      // private (live state) and exposing a setter lets the cap branch
      // be tested without a parallel-send harness.
      getActiveFileSends: () => activeFileSends,
      setActiveFileSends: (n) => { activeFileSends = n; },
      // The guild-member fetch cache is module-private; tests need to
      // reset it between cases so a prior test's cached members don't
      // mask a `members.fetch` rejection in the next test.
      memberFetchCache,
      // The ACK_TIMEOUT_MSG_RE fallback shape drives the failure_type
      // alarm — table-driven tests pin every shape so a silent regex
      // breakage can't slip through.
      isAckTimeoutError,
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
      // /qurl file + /qurl map handlers + flow constants. Tests pin
      // against production values via these exports rather than
      // re-stating the strings.
      handleQurlFile,
      handleQurlMap,
      resolveRecipientUsers,
      partitionRecipients,
      selfDestructOptionToSeconds,
      renderRecipientWarnings,
      renderConfirmCardContent,
      SEND_STAGE_AWAITING_CONFIRM,
      SEND_USER_SELECT_CUSTOM_ID,
      SEND_CONFIRM_SEND_CUSTOM_ID,
      SEND_CONFIRM_CANCEL_CUSTOM_ID,
      SEND_FLOW_TTL_SECONDS,
      SELF_DESTRUCT_NO_TIMER_CHOICE,
    },
  }),
};
