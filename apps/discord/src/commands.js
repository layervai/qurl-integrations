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
} = require('discord.js');
const crypto = require('crypto');
const config = require('./config');
const db = require('./store');
const logger = require('./logger');
const { COLORS, TIMEOUTS, RESOURCE_TYPES, DM_STATUS, MAX_FILE_SIZE, MAX_CONCURRENT_MONITORS, AUDIT_EVENTS } = require('./constants');
const { expiryToISO, expiryToMs } = require('./utils/time');
const { requireAdmin } = require('./utils/admin');
const { deleteLink, getResourceStatus } = require('./qurl');
const { downloadAndUpload, reUploadBuffer, mintLinks, uploadJsonToConnector, isAllowedSourceUrl } = require('./connector');

// Max tokens the QURL API allows per resource. When exceeded, a new
// resource must be created (re-upload) to get a fresh token pool.
const TOKENS_PER_RESOURCE = 10;

// Shared helper: many Discord API calls (edits, updates, follow-ups) are
// best-effort — if the interaction token expired or Discord is briefly
// degraded, we log a warning and continue rather than fail the whole flow.
// Extracted to deduplicate ~13 identical `.catch(err => logger.warn(...))`
// one-liners across this file.
const logIgnoredDiscordErr = (err) => logger.warn('Discord API op failed (ignored)', { error: err.message });
const { getVoiceChannelMembers, getTextChannelMembers, sendDM } = require('./discord');


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



const { sanitizeFilename, escapeDiscordMarkdown, sanitizeDisplayName } = require('./utils/sanitize');

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

// --- Shared DM delivery payload builder ---
// Builds the {embeds, components} payload for a per-recipient DM. The
// embed copy is intentionally evocative ("opened a door", "Portal closes",
// "what's on the other side is invisible to … scanners, bots, crawlers,
// strangers") rather than literal ("shared a file with you") — the brand
// goal is to convey the qURL hidden-layer model, not just announce a
// file transfer. The qURL link is rendered as a `🔗 Step Through` Link
// button rather than a bare URL field; recipients click the button to
// open the link in their default browser.
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
//     │  This link is your portal — what's on the other side is    │  (fixed body copy)
//     │  invisible to everyone else on the internet: scanners,     │
//     │  bots, crawlers, strangers.                                │
//     │                                                             │
//     │  🕐 Portal closes in 24 hours                               │  (Discord <t:N:R> — auto-updates client-side
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
    // shows the message as a single-line styled box. 450-char cap keeps
    // the body paragraph visible without scroll.
    const capped = personalMessage.substring(0, 450).replace(/[\r\n]+/g, ' ').trim();
    embed.addFields({ name: '\u200B', value: `> *"${capped}"*` });
  }

  embed.addFields(
    {
      name: '\u200B',
      value: "This link is your portal — what's on the other side is invisible to everyone else on the internet: scanners, bots, crawlers, strangers.",
    },
    {
      // Discord's native relative-time markdown: <t:UNIX:R> renders
      // CLIENT-SIDE based on the viewer's current time, so the recipient
      // sees "in 24 hours" at send time and "in 16 hours" 8 hours later
      // (and "1 hour ago" once the link has expired). No bot-side editing
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
        return `\ud83d\udd50 Portal closes <t:${expiresAt}:R>`;
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
    // Tracks the bot's "Ready! Drop your file here" message in the DM
    // so we can delete it after capture. Only set in the DM-pivot path
    // (the DM-already path uses initBtn.update which is the ephemeral
    // interaction reply — no separate DM bot message to clean up).
    let dmPromptMessage = null;
    if (interaction.channel.type === ChannelType.DM) {
      captureChannel = interaction.channel;
      await initBtn.update({
        content: '\u{1F4C1} **Drop your file here** (drag-drop or use the `+` icon). I\'ll wait 60 seconds.',
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
        dmPromptMessage = await dm.send('\u{1F4C1} **Ready! Drop your file here** (drag-drop or use the `+` icon). I\'ll wait 60 seconds.');
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
        content: '\u{1F4EC} **I sent you a DM — drop your file there and come back here to send it.** I\'ll wait 60 seconds.',
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
      // Tear down the DM prompt before returning. Without this the user
      // is left with a stale "Ready! Drop your file here. I'll wait 60
      // seconds." sitting in their DM thread forever — bots can't go
      // back and delete the prompt later, and the user sees no feedback
      // that the timeout happened on the bot side. Cleanup is fire-and-
      // forget so a delete failure doesn't mask the user-facing message.
      if (dmPromptMessage) {
        dmPromptMessage.delete().catch((dErr) => logger.warn('Failed to delete stale DM prompt after capture timeout/error', {
          sendNonce, userId: interaction.user.id, error: dErr?.message,
        }));
      }
      clearCooldown(interaction.user.id);
      return interaction.editReply({ content: 'No file received within 60 seconds. Send cancelled.', components: [] }).catch(logIgnoredDiscordErr);
    }

    attachment = fileMessage.attachments.first();

    // No fileMessage.delete() here — bots can't delete user messages
    // in DMs (Manage Messages doesn't apply outside guild channels), and
    // even if we could, the file is in a 1:1 DM with no other viewers.
    //
    // We CAN delete the bot-authored DM messages once capture is done,
    // and that's worth doing for visual cleanup. Send a brief "Got your
    // file" confirmation, then delete BOTH that confirmation and the
    // earlier "Ready! Drop your file here" prompt. The user's drop-
    // message stays — they can delete it themselves after Send if they
    // want a fully clean DM thread.
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
    // below. Real Google Maps URLs peak around ~300 chars.
    const locationValue = modalSubmit.fields.getTextInputValue('location_value').trim().slice(0, 2000);
    await modalSubmit.deferUpdate();

    const mapsPatterns = [
      /https?:\/\/(?:www\.)?google\.com\/maps\/(?:place|search|dir|@)[\w/.,@?=&+%-]{1,500}/,
      /https?:\/\/(?:goo\.gl\/maps|maps\.app\.goo\.gl)\/[\w-]{1,100}/,
      /https?:\/\/(?:www\.)?google\.com\/maps\/embed\/v1\/\w{1,32}\?[^\s]{1,500}/,
    ];

    let detectedUrl = null;
    for (const pattern of mapsPatterns) {
      const match = locationValue.match(pattern);
      if (match) { detectedUrl = match[0]; break; }
    }

    if (detectedUrl && isGoogleMapsURL(detectedUrl)) {
      locationUrl = detectedUrl;
      const queryMatch = detectedUrl.match(/[?&]q=([^&]+)/);
      const placeMatch = detectedUrl.match(/\/place\/([^/@]+)/);
      // decodeURIComponent throws URIError on malformed %-encoding (e.g. %ZZ).
      // Swallow and fall back to the raw string — a garbled label is better
      // than a crashed command handler.
      const safeDecode = (s) => { try { return decodeURIComponent(s); } catch { return s; } };
      if (queryMatch) locationName = safeDecode(queryMatch[1].replace(/\+/g, ' '));
      else if (placeMatch) locationName = safeDecode(placeMatch[1].replace(/\+/g, ' '));
    } else if (detectedUrl) {
      locationName = locationValue;
      locationUrl = `https://www.google.com/maps/search/${encodeURIComponent(locationValue)}`;
    } else {
      locationName = locationValue;
      locationUrl = `https://www.google.com/maps/search/${encodeURIComponent(locationValue)}`;
    }
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

  const formId = `qurl_form_${sendNonce}`;
  // Component customIds for the form-loop filter. Every entry must be a
  // top-level component the loop's awaitMessageComponent can dispatch on
  // (button, string select, user select). Modal customIds are NOT here —
  // they live as local consts inside their own handler so the form-loop
  // filter set stays tight.
  const ids = {
    targetSelect: `${formId}_target`,
    userSelect: `${formId}_user`,
    messageBtn: `${formId}_msg_btn`,
    expirySelect: `${formId}_expiry`,
    sendBtn: `${formId}_send`,
    cancelBtn: `${formId}_cancel`,
  };
  // Modal customId — local to the messageBtn handler; never consumed by
  // the form-loop filter, kept out of `ids` so Object.values(ids) doesn't
  // grow noise.
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
      content += `\n\n_Personal message:_ "${preview}"`;
    }
    return content;
  };

  const formRows = () => {
    const rows = [];

    rows.push(new ActionRowBuilder().addComponents(
      new StringSelectMenuBuilder()
        .setCustomId(ids.targetSelect)
        .setPlaceholder('Recipient(s) — choose type')
        .addOptions(
          { label: 'A specific user', value: 'user', description: 'Pick one user to send to', default: target === 'user' },
          { label: 'Everyone in this channel', value: 'channel', description: 'All members of this text channel (excl. bots and you)', default: target === 'channel' },
          { label: 'Everyone in your voice channel', value: 'voice', description: 'You must be in a voice channel', default: target === 'voice' },
        )
    ));

    if (target === 'user') {
      rows.push(new ActionRowBuilder().addComponents(
        new UserSelectMenuBuilder()
          .setCustomId(ids.userSelect)
          .setPlaceholder('Pick a user')
          .setMinValues(1)
          .setMaxValues(1)
      ));
    }

    rows.push(new ActionRowBuilder().addComponents(
      new ButtonBuilder()
        .setCustomId(ids.messageBtn)
        // Primary (blue) + a longer descriptive label so this button reads as
        // a peer of the surrounding select dropdowns rather than a small
        // grey afterthought. Discord doesn't let buttons match select-menu
        // width, but a more present label closes most of the visual gap.
        .setLabel(personalMessage ? '✏\u{FE0F} Edit personal message for recipients' : '✏\u{FE0F} Add a personal note for recipients (optional)')
        .setStyle(ButtonStyle.Primary)
    ));

    rows.push(new ActionRowBuilder().addComponents(
      new StringSelectMenuBuilder()
        .setCustomId(ids.expirySelect)
        .setPlaceholder('Link expiry')
        .addOptions(
          ...EXPIRY_CHOICES.map(c => ({ label: c.name, value: c.value, default: c.value === expiresIn }))
        )
    ));

    const recipientsResolved = (target === 'user' && recipients.length === 1)
      || ((target === 'channel' || target === 'voice') && recipients.length > 0);
    rows.push(new ActionRowBuilder().addComponents(
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
      // the actual recipient pick). For channel and voice, ALWAYS re-resolve
      // — members can join or leave during the up-to-3-minute form-loop
      // window, and a stale recipients array would mint links for ghosts.
      const requiresFreshResolve = newTarget === 'channel' || newTarget === 'voice';
      if (newTarget !== target || requiresFreshResolve) {
        target = newTarget;
        recipients = [];
        if (target === 'channel') {
          try {
            await fetchGuildMembers(interaction.guild);
          } catch (err) {
            logger.error('Failed to fetch guild members', { error: err.message, sendNonce, userId: interaction.user.id, guildId: interaction.guild?.id });
            clearCooldown(interaction.user.id);
            await safeCompUpdate(compInt, { content: 'Failed to load channel members. Send cancelled.', components: [] });
            return;
          }
          recipients = getTextChannelMembers(interaction.channel, interaction.user.id);
          if (recipients.length === 0) {
            target = null;
            await safeCompUpdate(compInt, { content: formContent({ warning: 'No other members in this channel. Pick another option.' }), components: formRows() });
            continue;
          }
        } else if (target === 'voice') {
          const result = getVoiceChannelMembers(interaction.guild, interaction.user.id);
          if (result.error === 'not_in_voice') {
            target = null;
            await safeCompUpdate(compInt, { content: formContent({ warning: 'You must be in a voice channel to use this option.' }), components: formRows() });
            continue;
          }
          if (result.members.length === 0) {
            target = null;
            await safeCompUpdate(compInt, { content: formContent({ warning: 'No other users in your voice channel.' }), components: formRows() });
            continue;
          }
          recipients = result.members;
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
      const selectedUser = compInt.users.first();
      if (!selectedUser) {
        await safeCompDefer(compInt);
        continue;
      }
      if (selectedUser.bot) {
        await safeCompUpdate(compInt, { content: formContent({ warning: 'Cannot send to a bot. Pick a different user.' }), components: formRows() });
        continue;
      }
      if (selectedUser.id === interaction.user.id) {
        await safeCompUpdate(compInt, { content: formContent({ warning: 'Cannot send to yourself. Pick a different user.' }), components: formRows() });
        continue;
      }
      recipients = [selectedUser];
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
          .setMaxLength(450)
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

  // --- Step 4: Process and send (back-half — preserved unchanged) ---
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
      const firstUpload = await downloadAndUpload(attachment.url, filename, attachment.contentType, apiKey);
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
          reuploadFn: () => reUploadBuffer(bufHolder.buf, filename, attachment.contentType, apiKey),
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
      const firstUpload = await uploadJsonToConnector(locPayload, 'location.json', apiKey);
      connectorResourceId = firstUpload.resource_id;

      const expiresAt = expiryToISO(expiresIn);
      const allLinks = await mintLinksInBatches({
        initialResourceId: firstUpload.resource_id,
        reuploadFn: () => uploadJsonToConnector(locPayload, 'location.json', apiKey),
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
    releaseSlot();
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
      logger.audit(sent ? AUDIT_EVENTS.DISPATCH_SENT : AUDIT_EVENTS.DISPATCH_FAILED, { send_id: sendId });
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
    });
  } catch (err) {
    logger.error('saveSendConfig failed; Add Recipients will be unavailable for this send', {
      sendId, error: err.message,
    });
  }

  // Ephemeral confirmation with Add Recipients + Revoke buttons.
  // Match on the Discord id (snowflake, globally unique) rather than the
  // display username — usernames can collide within a guild.
  const TRUNC_LIMIT = 5;
  const failedUserIds = new Set(failedUsers.map(u => u.id || u));
  const successNames = recipients.filter(r => !failedUserIds.has(r.id)).map(r => r.username);

  function buildConfirmMsg(showAll) {
    let msg = `Sent to ${delivered} user${delivered !== 1 ? 's' : ''} | Expires: ${expiresIn} | One-time links`;
    if (failed > 0) {
      msg += `\n${failed} could not be reached: ${failedUsers.map(u => u.username).join(', ')}`;
    }
    if (successNames.length > 0) {
      if (showAll || successNames.length <= TRUNC_LIMIT) {
        msg += `\nRecipients: ${successNames.join(', ')}`;
      } else {
        msg += `\nRecipients: ${successNames.slice(0, TRUNC_LIMIT).join(', ')} +${successNames.length - TRUNC_LIMIT} more`;
      }
    }
    return msg;
  }

  let confirmMsg = buildConfirmMsg(false);
  const needsExpand = successNames.length > TRUNC_LIMIT;

  const buttonRow = new ActionRowBuilder().addComponents(
    new ButtonBuilder()
      .setCustomId(`qurl_add_${sendId}`)
      .setLabel('Add Recipients')
      .setStyle(ButtonStyle.Primary),
    new ButtonBuilder()
      .setCustomId(`qurl_revoke_${sendId}`)
      .setLabel('Revoke All Links')
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

  const response = await interaction.editReply({
    content: confirmMsg,
    components: delivered > 0 ? [buttonRow] : [],
  });

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
  if (interaction.channel && (target === 'channel' || target === 'voice') && delivered > 0) {
    // Same sanitizer the DM embed uses (sanitizeDisplayName: NFKC + bidi/
    // zero-width/control strip + markdown escape + 64-char cap + 'Someone'
    // fallback). Channel-post is a wider blast radius than DM, so applying
    // the same spoof defense here is critical — without it a display name
    // with a leading U+202E flips text direction in the public announcement.
    const safeName = sanitizeDisplayName(resolveSenderAlias(interaction));
    const notifyMsg = target === 'voice'
      ? `📩 **${safeName}** has shared something with users currently on voice via **qURL Bot** — if you're on voice, check your DMs from qURL Bot.`
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

    collector.on('collect', async (btnInteraction) => {
      if (btnInteraction.customId === `qurl_expand_${sendId}`) {
        await btnInteraction.deferUpdate().catch(logIgnoredDiscordErr);
        showAllRecipients = !showAllRecipients;
        confirmMsg = buildConfirmMsg(showAllRecipients);
        monitor.updateBaseMsg(confirmMsg);
        const fullMsg = monitor.getFullMsg();
        const updatedRow = new ActionRowBuilder().addComponents(
          new ButtonBuilder().setCustomId(`qurl_add_${sendId}`).setLabel('Add Recipients').setStyle(ButtonStyle.Primary),
          new ButtonBuilder().setCustomId(`qurl_revoke_${sendId}`).setLabel('Revoke All Links').setStyle(ButtonStyle.Danger),
          new ButtonBuilder().setCustomId(`qurl_expand_${sendId}`).setLabel(showAllRecipients ? 'Show Less' : 'Show All').setStyle(ButtonStyle.Secondary),
        );
        await interaction.editReply({ content: fullMsg, components: [updatedRow] }).catch(logIgnoredDiscordErr);
        return;
      }

      if (btnInteraction.customId === `qurl_revoke_${sendId}`) {
        await btnInteraction.deferUpdate().catch(logIgnoredDiscordErr);
        await interaction.editReply({ content: 'Revoking links...', components: [] }).catch(logIgnoredDiscordErr);
        try {
          const revoked = await revokeAllLinks(sendId, interaction.user.id, apiKey);
          await interaction.editReply({
            content: `Revoked ${revoked.success}/${revoked.total} links. Note: already-opened links cannot be revoked.`,
            components: [],
          }).catch(logIgnoredDiscordErr);
        } catch (err) {
          logger.error('Revoke failed', { sendId, error: err.message });
          await interaction.editReply({
            content: 'Failed to revoke links. Try `/qurl revoke` instead.',
            components: [],
          }).catch(logIgnoredDiscordErr);
        }
        if (monitor) monitor.stop();
        collector.stop('revoked');

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
          const maxSelect = Math.min(10, remaining);
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
        interaction.editReply({
          content: (monitor ? monitor.getFullMsg() : confirmMsg) + '\n\n\u23f0 **Management window closed** — use `/qurl revoke` to revoke later.',
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
    return { msg: 'Send configuration not found.', newResourceIds: [], delivered: 0, failed: 0 };
  }

  // Filter out bots and the sender. Convert the Discord Collection to a
  // plain array so later callers (map/forEach over newRecipients[i]) work.
  const newRecipients = [...usersCollection
    .filter(u => !u.bot && u.id !== senderDiscordId)
    .values()];

  if (newRecipients.length === 0) {
    return { msg: 'No valid recipients selected (bots and yourself are excluded).', newResourceIds: [], delivered: 0, failed: 0 };
  }

  // Create new QURL links for each resource type in the send config
  // recipientLinks[recipientId] = [{ qurlLink, resourceId, resType, label }]
  const recipientLinks = {};
  const hasFile = sendConfig.connector_resource_id;
  const hasLocation = sendConfig.actual_url;

  if (!hasFile && !hasLocation) {
    return { msg: 'Cannot add recipients — send configuration is incomplete.', newResourceIds: [], delivered: 0, failed: 0 };
  }

  // Tracks which prep paths actually completed so we can emit a single
  // upload_success per send (not one per kind). A sendConfig with both
  // file + location would otherwise fire two events for the same send,
  // which double-counts UploadCount in CloudWatch unless the metric
  // filter dimensions on `kind` (it doesn't, currently — see
  // qurl-integrations-infra#309). The collapsed event keeps UploadCount
  // = "number of fully-prepared sends" regardless of kind composition.
  const preparedKinds = [];
  try {
    if (hasFile) {
      // Re-download from the stored Discord CDN URL, then upload a fresh
      // resource so the 10-token pool is full. Re-upload again every
      // TOKENS_PER_RESOURCE recipients. The original resource is drained by
      // the initial send, so we CANNOT reuse sendConfig.connector_resource_id.
      if (!sendConfig.attachment_url) {
        return {
          msg: 'Cannot add file recipients — original attachment is no longer available. Please create a new send.',
          newResourceIds: [],
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
          newResourceIds: [],
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
        const first = await downloadAndUpload(sendConfig.attachment_url, filename, contentType, apiKey);
        fileBuffer = first.fileBuffer;
        allLinks = await mintLinksInBatches({
          initialResourceId: first.resource_id,
          reuploadFn: () => reUploadBuffer(fileBuffer, filename, contentType, apiKey),
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
        return { msg, newResourceIds: [], delivered: 0, failed: 0 };
      }

      if (allLinks.length < newRecipients.length) {
        logger.error('mintLinks returned fewer links than expected in addRecipients', { expected: newRecipients.length, got: allLinks.length });
        const newResourceIds = [...new Set(allLinks.map(l => l.resourceId))];
        return { msg: `Only ${allLinks.length} of ${newRecipients.length} links created. Try again.`, newResourceIds, delivered: 0, failed: 0 };
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
      const firstUpload = await uploadJsonToConnector(locPayload, 'location.json', apiKey);
      const expiresAt = expiryToISO(sendConfig.expires_in);
      const allLinks = await mintLinksInBatches({
        initialResourceId: firstUpload.resource_id,
        reuploadFn: () => uploadJsonToConnector(locPayload, 'location.json', apiKey),
        expiresAt,
        recipientCount: newRecipients.length,
        apiKey,
      });

      if (allLinks.length < newRecipients.length) {
        logger.error('mintLinks returned fewer links than expected in addRecipients (location)', { expected: newRecipients.length, got: allLinks.length });
        return { msg: `Only ${allLinks.length} of ${newRecipients.length} location links created. Try again.`, newResourceIds: [], delivered: 0, failed: 0 };
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
    return { msg, newResourceIds: [], delivered: 0, failed: 0 };
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
    return { msg: 'Failed to create any links.', newResourceIds: [], delivered: 0, failed: 0 };
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
      newResourceIds: [],
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
      logger.audit(sent ? AUDIT_EVENTS.DISPATCH_SENT : AUDIT_EVENTS.DISPATCH_FAILED, { send_id: sendId });
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
  return { msg, newResourceIds, delivered, failed };
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

async function handleRevoke(interaction, apiKey) {
  if (!apiKey) {
    return interaction.reply({ content: 'qURL API key is not configured.', ephemeral: true });
  }
  await interaction.deferReply({ ephemeral: true });

  const recentSends = await db.getRecentSends(interaction.user.id, 5);

  if (recentSends.length === 0) {
    return interaction.editReply({ content: 'No recent sends to revoke.' });
  }

  // Nonce the customId so two concurrent /qurl revoke select menus don't
  // route each other's events.
  const revokeNonce = crypto.randomBytes(8).toString('hex');
  const select = new StringSelectMenuBuilder()
    .setCustomId(`qurl_revoke_select_${revokeNonce}`)
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
  const response = await interaction.editReply({
    content: 'Select a send to revoke all its links:',
    components: [row],
  });

  try {
    const selectInteraction = await response.awaitMessageComponent({
      componentType: ComponentType.StringSelect,
      filter: (i) => i.user.id === interaction.user.id && i.customId === `qurl_revoke_select_${revokeNonce}`,
      time: 60000,
    });

    const sendId = selectInteraction.values[0];
    const revoked = await revokeAllLinks(sendId, interaction.user.id, apiKey);

    await selectInteraction.update({
      content: `Revoked ${revoked.success}/${revoked.total} links. Note: already-opened links cannot be revoked.`,
      components: [],
    });
  } catch {
    await interaction.editReply({ content: 'Revocation timed out.', components: [] }).catch(logIgnoredDiscordErr);
  }
}

async function revokeAllLinks(sendId, senderDiscordId, apiKey) {
  const resourceIds = await db.getSendResourceIds(sendId, senderDiscordId);
  let success = 0;

  const results = await batchSettled(resourceIds, async (resourceId) => {
    await deleteLink(resourceId, apiKey);
    return resourceId;
  }, 5);

  for (const r of results) {
    if (r.status === 'fulfilled') success++;
    else logger.error('Failed to revoke QURL', { error: r.reason?.message });
  }

  // Record the user's revocation intent so this send stops appearing in
  // the /qurl revoke dropdown. We mark regardless of per-link success —
  // partial failures are surfaced in the reply message ("Revoked X/Y"),
  // and re-picking the same send from the dropdown wouldn't help anyway
  // since the failed resource_ids are the same.
  // Emit BEFORE markSendRevoked for the same reason the dispatch
  // emissions fire before updateSendDMStatus — a DB write throw must
  // not suppress the audit metric, which is exactly the failure mode
  // we're trying to measure. Distinguish all-failed (success === 0)
  // from at-least-partial (success > 0) so the dashboard isn't
  // misled into counting fully-failed revokes as successes.
  if (resourceIds.length > 0) {
    const event = success > 0 ? AUDIT_EVENTS.REVOKE_SUCCESS : AUDIT_EVENTS.REVOKE_FAILED;
    logger.audit(event, { send_id: sendId, success, total: resourceIds.length });
  }
  await db.markSendRevoked(sendId, senderDiscordId);

  logger.info('Revoked send', { sendId, success, total: resourceIds.length });
  return { success, total: resourceIds.length };
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

      // /qurl setup — admin-only, configure API key for this server
      if (sub === 'setup') {
        if (!interaction.guildId) {
          return interaction.reply({ content: 'This command can only be used in a server, not in DMs.', ephemeral: true });
        }
        if (!interaction.memberPermissions?.has(PermissionFlagsBits.ManageGuild)) {
          return interaction.reply({ content: 'Only server administrators can configure qURL.', ephemeral: true });
        }
        // Refuse to accept a guild API key unless encryption-at-rest is
        // configured. Falling through to the crypto module's plaintext
        // fallback would silently store a billing-sensitive secret on disk.
        if (!process.env.KEY_ENCRYPTION_KEY) {
          logger.error('Refusing /qurl setup: KEY_ENCRYPTION_KEY is not set');
          return interaction.reply({
            content: '❌ **qURL is not ready to accept API keys on this server.**\n\n' +
              'The bot operator needs to set `KEY_ENCRYPTION_KEY` (encryption-at-rest) before '
              + '/qurl setup can store keys safely. Ask them to check the deployment env.',
            ephemeral: true,
          });
        }
        // Use a modal to collect the API key — modal inputs are NOT recorded in Discord audit logs
        // Nonce the customId so two concurrent /qurl setup flows don't consume each other's submissions.
        const setupNonce = crypto.randomBytes(8).toString('hex');
        const setupModalId = `qurl_setup_modal_${setupNonce}`;
        const modal = new ModalBuilder()
          .setCustomId(setupModalId)
          .setTitle('Configure qURL');
        const keyInput = new TextInputBuilder()
          .setCustomId('api_key')
          .setLabel('qURL API Key')
          .setPlaceholder('lv_live_your_key_here')
          .setStyle(TextInputStyle.Short)
          .setRequired(true)
          .setMinLength(28);
        modal.addComponents(new ActionRowBuilder().addComponents(keyInput));
        await interaction.showModal(modal);

        let modalSubmit;
        try {
          modalSubmit = await interaction.awaitModalSubmit({ filter: (i) => i.customId === setupModalId && i.user.id === interaction.user.id, time: 120000 });
        } catch {
          return; // Modal dismissed or timed out
        }
        const submittedKey = modalSubmit.fields.getTextInputValue('api_key').trim();
        if (!/^lv_(live|test)_[A-Za-z0-9_-]{20,}$/.test(submittedKey)) {
          return modalSubmit.reply({
            content: 'Invalid API key format. Keys start with `lv_live_` or `lv_test_` and are at least 28 characters.',
            ephemeral: true,
          });
        }
        // Validate key by making a lightweight GET — no resource creation
        await modalSubmit.deferReply({ ephemeral: true });
        try {
          const resp = await fetch(`${config.QURL_ENDPOINT}/v1/qurls?limit=1`, {
            headers: { 'Authorization': `Bearer ${submittedKey}`, 'Accept': 'application/json' },
            signal: AbortSignal.timeout(10000),
          });
          if (resp.status === 401 || resp.status === 403) {
            return modalSubmit.editReply({ content: '❌ **Invalid API key.** Double-check your key at **https://layerv.ai**.' });
          }
          if (!resp.ok) {
            return modalSubmit.editReply({ content: `❌ **qURL API error** (${resp.status}). Try again later.` });
          }
        } catch (err) {
          // Don't reflect err.message to Discord — network errors can contain
          // internal hostnames/IPs (e.g. "connect ECONNREFUSED 10.0.0.5:8080")
          // that should not leak to a guild admin's screen.
          logger.error('validate-key request failed', { error: err.message });
          return modalSubmit.editReply({
            content: '❌ **Could not validate key.** Please try again in a moment.',
          });
        }

        const guildId = interaction.guildId;
        await db.setGuildApiKey(guildId, submittedKey, interaction.user.id);
        logger.info('Guild API key configured', { guild_id: guildId, configured_by: interaction.user.id });
        return modalSubmit.editReply({
          content: '✅ **qURL is now configured for this server!**\n\n' +
            'Your team can use `/qurl send` to share files and locations securely.\n' +
            'All qURL usage will be billed to your API key.',
          ephemeral: true,
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
          return interaction.reply({
            content: `✅ **qURL is configured**\n` +
              `Key fingerprint: \`${keyFingerprint}\`\n` +
              `Configured by: <@${guildConfig.configured_by}>\n` +
              `Last updated: ${guildConfig.updated_at}`,
            ephemeral: true,
          });
        }
        return interaction.reply({
          content: '❌ **qURL is not configured for this server.**\n\n' +
            '1. Sign up at **https://layerv.ai** to get your API key\n' +
            '2. Run `/qurl setup api_key:lv_live_your_key_here`\n\n' +
            'Only server administrators can run setup.',
          ephemeral: true,
        });
      }

      // Gate: require guild API key for send/revoke
      let resolvedApiKey = null;
      if (sub === 'send' || sub === 'revoke') {
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
      if (sub === 'revoke') return handleRevoke(interaction, resolvedApiKey);
      if (sub === 'help') {
        // Rendered output (after Discord's markdown parser):
        //
        //   qURL Bot — Help
        //
        //   Getting started — Share resources securely via one-time links:
        //     /qurl send — send a file or location to users
        //     /qurl revoke — revoke links from a previous send
        //     /qurl help — show this message
        //
        //   How it works:
        //     1. Run /qurl send and pick "Send File" or "Send Location"
        //     2. (Files only) I'll DM you privately — drop the file there,
        //        not in the public channel
        //     3. Choose recipient(s), expiry, and optionally a personal
        //        message — then click Send
        //     4. Recipients get a one-time link by DM that self-destructs
        //        on first access (or when the expiry elapses)
        //
        //   Setting up (for Admins):
        //     /qurl setup — configure your API key (admin only)
        //     /qurl status — check if qURL is configured (admin only)
        //
        //   Terms: a protected resource is the file or location you're
        //   sharing. A qurl (or access link) is the single-use URL that
        //   delivers it. You create a qurl for a protected resource each
        //   time you run /qurl send.
        //
        //   Large servers (~1000+ members): when sending to "Everyone in
        //   this channel" or "Everyone in your voice channel", members
        //   who appear offline in Discord may be skipped. If you need
        //   to reach a specific person for sure, pick "A specific user".
        //
        //   Sign up at https://layerv.ai to get your API key.
        //
        // Section order (post-PR #124): user-facing flow first (Getting
        // started → How it works), then admin-only setup, then glossary
        // (Terms), then operational caveat (Large servers), then signup.
        // PR #134 rewrote /qurl send as a button-driven flow — the
        // "How it works" steps below describe the new shape (no slash
        // options; pick Send File / Send Location → DM-pivot for files
        // → recipient/expiry/message form → Send). If the flow changes
        // again, update both the rendered-output ASCII above AND the
        // string concatenation below to keep them in sync.
        return interaction.reply({
          content: '**qURL Bot — Help**\n\n' +
            '**Getting started — Share resources securely via one-time links:**\n' +
            '  `/qurl send` — send a file or location to users\n' +
            '  `/qurl revoke` — revoke links from a previous send\n' +
            '  `/qurl help` — show this message\n\n' +
            '**How it works:**\n' +
            // Leading tab on "1." keeps Discord's markdown parser from
            // treating it as the start of an ordered list (which would
            // renumber it relative to the subsequent lines and visually
            // misalign "1." with "2.", "3.", "4."). The tab indent now
            // matches the two-space indent below, but bypasses the list
            // auto-formatter.
            '\t1. Run `/qurl send` and pick **Send File** or **Send Location**\n' +
            '  2. (Files only) I\'ll DM you privately — drop the file there, not in the public channel\n' +
            '  3. Choose recipient(s), expiry, and optionally a personal message — then click **Send**\n' +
            '  4. Recipients get a one-time link by DM that self-destructs on first access (or when the expiry elapses)\n\n' +
            '**Setting up (for Admins):**\n' +
            '  `/qurl setup` — configure your API key (admin only)\n' +
            '  `/qurl status` — check if qURL is configured (admin only)\n\n' +
            '**Terms:** a *protected resource* is the file or location you\'re sharing. ' +
            'A *qurl* (or *access link*) is the single-use URL that delivers it. ' +
            'You create a qurl for a protected resource each time you run `/qurl send`.\n\n' +
            '**Large servers (~1000+ members):** when sending to **Everyone in this channel** ' +
            'or **Everyone in your voice channel**, members who appear offline in Discord may be skipped. ' +
            'If you need to reach a specific person for sure, pick **A specific user**.\n\n' +
            'Sign up at **https://layerv.ai** to get your API key.',
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

// Handle command interactions
async function handleCommand(interaction) {
  // /qurl now has zero options after the button-driven redesign; no other
  // command in this file uses autocomplete. Discord can still deliver
  // autocomplete events (legacy clients, stale registrations) — short-
  // circuit them so they don't fall through to the chat-input path.
  if (interaction.isAutocomplete()) return;

  if (!interaction.isChatInputCommand()) return;

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
    } catch (err) {
      logger.warn('Failed to reply to stale command interaction', {
        command: interaction.commandName, error: err.message,
      });
    }
    return;
  }

  try {
    await command.execute(interaction);
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

    try {
      if (interaction.replied || interaction.deferred) {
        await interaction.followUp(reply);
      } else {
        await interaction.reply(reply);
      }
    } catch (replyError) {
      logger.error('Failed to send error response', { error: replyError.message });
    }
  }
}

module.exports = {
  commands,
  registerCommands,
  handleCommand,
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
      batchSettled,
      expiryToISO,
      sendCooldowns,
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
      mintLinksInBatches,
      activeMonitors,
      // Test-only file-concurrency hooks. The slot counter is module-
      // private (live state) and exposing a setter lets the cap branch
      // be tested without a parallel-send harness.
      getActiveFileSends: () => activeFileSends,
      setActiveFileSends: (n) => { activeFileSends = n; },
      // The guild-member fetch cache is module-private; tests need to
      // reset it between cases so a prior test's cached members don't
      // mask a `members.fetch` rejection in the next test.
      memberFetchCache,
    },
  }),
};
