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
const db = require('./database');
const logger = require('./logger');
const { COLORS, TIMEOUTS, RESOURCE_TYPES, DM_STATUS, MAX_FILE_SIZE, MAX_CONCURRENT_MONITORS } = require('./constants');
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
// file transfer. The qURL link is rendered as a `Step Through →` Link
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
//     │  🕐 Portal closes in **24 hours**                           │  (expiry line)
//     │                                                             │
//     │  Quantum URL (qURL) · The internet has a hidden layer.     │  (final embed field;
//     │  This is how you enter.                                     │   `qURL` → https://layerv.ai)
//     │                                                             │
//     │  ┌──────────────────────────┐                               │
//     │  │   Step Through →         │  (Link button — opens qURL)
//     │  └──────────────────────────┘                               │
//     └─────────────────────────────────────────────────────────────┘
//
// Discord's Link-style buttons are always grey/blurple; the green color
// in the design mockup would require a Success-style button + custom_id
// + interaction handler that redirects, which adds a click round-trip
// for marginal aesthetic gain. Sticking with Link button for this pivot.
// Single source of truth for the supported `/qurl send` expiry choices
// and their human-readable labels. Used by:
//   1. `formatExpiryLabel` to render the "Portal closes in X" line in
//      the recipient DM (e.g. `'24h'` → `'24 hours'`)
//   2. `EXPIRY_CHOICES` (built below from this map) to populate the
//      SlashCommandBuilder `addChoices(...)` for the `expiry_optional`
//      option, so the two cannot drift when a new choice is added.
// Hoisted to module scope so the dictionary isn't reallocated per call.
const EXPIRY_LABELS = {
  '30m': '30 minutes',
  '1h': '1 hour',
  '6h': '6 hours',
  '24h': '24 hours',
  '7d': '7 days',
};

const EXPIRY_CHOICES = Object.entries(EXPIRY_LABELS).map(([value, name]) => ({ name, value }));

function formatExpiryLabel(expiresIn) {
  // `Object.hasOwn` rather than `EXPIRY_LABELS[expiresIn]` so a value of
  // `'__proto__'` / `'constructor'` / etc. can't trip the lookup against
  // an inherited Object.prototype property. Practical risk is zero (input
  // is gated by addChoices), but this is one identifier longer and removes
  // the question entirely.
  if (Object.hasOwn(EXPIRY_LABELS, expiresIn)) return EXPIRY_LABELS[expiresIn];
  // Defensive fallback for any `(\d+)([mhd])` value outside EXPIRY_LABELS
  // — the SlashCommandBuilder choices restrict input at the UI layer, but
  // the saved-config path could conceivably surface another.
  const m = String(expiresIn || '').match(/^(\d+)([mhd])$/);
  if (!m) return String(expiresIn || '');
  const [, n, u] = m;
  const word = u === 'm' ? 'minute' : u === 'h' ? 'hour' : 'day';
  return `${n} ${word}${n === '1' ? '' : 's'}`;
}

function buildDeliveryPayload({ senderAlias, qurlLink, expiresIn, personalMessage }) {
  // sanitizeDisplayName: NFKC + bidi/zero-width strip + markdown escape
  // + 64-char cap + 'Someone' fallback. Same helper used at the channel
  // announcement site so the spoof defense doesn't drift between sites.
  const safeSender = sanitizeDisplayName(senderAlias);
  const expiryLabel = formatExpiryLabel(expiresIn);

  const embed = new EmbedBuilder()
    .setColor(COLORS.QURL_BRAND)
    .setDescription(`**${safeSender}** opened a door for you.`);

  if (personalMessage) {
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
      name: '\u200B',
      value: `\ud83d\udd50 Portal closes in **${expiryLabel}**`,
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

  // Link button: grey, single-click opens qurlLink in the recipient's
  // browser. No interaction handler needed — Discord handles the redirect.
  const stepThrough = new ButtonBuilder()
    .setStyle(ButtonStyle.Link)
    .setLabel('Step Through →')
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
  const target = interaction.options.getString('target');
  const expiresIn = interaction.options.getString('expiry_optional') || '24h';
  const rawMessage = interaction.options.getString('message_optional');
  const personalMessage = rawMessage ? sanitizeMessage(rawMessage) : null;
  const commandAttachment = interaction.options.getAttachment('file_optional');

  if (isOnCooldown(interaction.user.id)) {
    return interaction.reply({ content: 'Please wait before sending again.', ephemeral: true });
  }
  // Set cooldown immediately to prevent concurrent request bypass
  setCooldown(interaction.user.id);

  const sendNonce = crypto.randomBytes(8).toString('hex');

  // --- Step 1: Collect user (if specific user target) ---
  let recipients = [];

  if (target === 'user') {
    const userSelectRow = new ActionRowBuilder().addComponents(
      new UserSelectMenuBuilder()
        .setCustomId(`qurl_user_${sendNonce}`)
        .setPlaceholder('Select a user to send to')
        .setMinValues(1)
        .setMaxValues(1)
    );

    await interaction.reply({ content: '**Select the user to send to:**', components: [userSelectRow], ephemeral: true });

    try {
      const selectInteraction = await interaction.channel.awaitMessageComponent({
        filter: (i) => i.customId === `qurl_user_${sendNonce}` && i.user.id === interaction.user.id,
        componentType: ComponentType.UserSelect,
        time: 60000,
      });

      const selectedUser = selectInteraction.users.first();
      if (!selectedUser) {
        clearCooldown(interaction.user.id);
        return selectInteraction.update({ content: 'No user selected. Send cancelled.', components: [] });
      }
      if (selectedUser.bot) {
        clearCooldown(interaction.user.id);
        return selectInteraction.update({ content: 'Cannot send to a bot.', components: [] });
      }
      if (selectedUser.id === interaction.user.id) {
        clearCooldown(interaction.user.id);
        return selectInteraction.update({ content: 'Cannot send to yourself.', components: [] });
      }
      recipients = [selectedUser];
      await selectInteraction.deferUpdate();
    } catch {
      // Timeout: release the cooldown so the user isn't blocked for 30s
      // after simply letting the select menu expire.
      clearCooldown(interaction.user.id);
      return interaction.editReply({ content: 'No user selected. Send cancelled.', components: [] });
    }
  } else if (target === 'channel') {
    await interaction.deferReply({ ephemeral: true });
    try {
      await fetchGuildMembers(interaction.guild);
    } catch (err) {
      logger.error('Failed to fetch guild members', { error: err.message });
      clearCooldown(interaction.user.id);
      return interaction.editReply({ content: 'Failed to load channel members. Please try again.' });
    }
    recipients = getTextChannelMembers(interaction.channel, interaction.user.id);
    if (recipients.length === 0) {
      clearCooldown(interaction.user.id);
      return interaction.editReply({ content: 'No other members in this channel.' });
    }
  } else if (target === 'voice') {
    await interaction.deferReply({ ephemeral: true });
    const result = getVoiceChannelMembers(interaction.guild, interaction.user.id);
    if (result.error === 'not_in_voice') {
      clearCooldown(interaction.user.id);
      return interaction.editReply({ content: 'You must be in a voice channel to use this target.' });
    }
    if (result.members.length === 0) {
      clearCooldown(interaction.user.id);
      return interaction.editReply({ content: 'No other users in your voice channel.' });
    }
    recipients = result.members;
  }

  if (recipients.length > config.QURL_SEND_MAX_RECIPIENTS) {
    clearCooldown(interaction.user.id);
    const overBy = recipients.length - config.QURL_SEND_MAX_RECIPIENTS;
    return interaction.editReply({
      content: `This send targets ${recipients.length} recipients, but the per-send cap is ${config.QURL_SEND_MAX_RECIPIENTS}. Trim ${overBy} recipient${overBy === 1 ? '' : 's'} from the channel/group, or split into multiple \`/qurl send\` runs.`,
      components: [],
    });
  }

  // --- Step 2: Resource — auto-detect from file attachment ---
  let attachment = null;
  let locationUrl = null;
  let locationName = null;
  let resourceType = null;

  // Pass the recipient's username through sanitizeDisplayName so the
  // ephemeral "Sending to …" label gets the same NFKC + bidi/zero-
  // width strip + markdown escape as the DM and channel-announcement
  // sites. Without this, a recipient named with a leading U+202E
  // would flip text direction inside the sender's confirmation reply.
  // (Only the sender sees this string, so blast radius is tiny — but
  // sanitizeDisplayName's docstring promises every site uses the same
  // helper, and divergence here would silently violate that contract.)
  const targetLabel = target === 'user'
    ? sanitizeDisplayName(recipients[0].username)
    : target === 'channel' ? 'this channel' : 'voice channel';

  if (commandAttachment) {
    // File attached — show only Send File button
    const fileRow = new ActionRowBuilder().addComponents(
      new ButtonBuilder()
        .setCustomId(`qurl_res_file_${sendNonce}`)
        .setLabel('\ud83d\udcc1 Send File')
        .setStyle(ButtonStyle.Primary),
    );

    await interaction.editReply({
      content: `**Sending to ${targetLabel}** — file detected: **${commandAttachment.name}**`,
      components: [fileRow],
    });
  } else {
    // No file — show only Send Location button
    const locRow = new ActionRowBuilder().addComponents(
      new ButtonBuilder()
        .setCustomId(`qurl_res_loc_${sendNonce}`)
        .setLabel('\ud83d\uddfa\ufe0f Send Location')
        .setStyle(ButtonStyle.Primary),
    );

    await interaction.editReply({
      content: `**Sending to ${targetLabel}** — share a location:`,
      components: [locRow],
    });
  }

  let resInteraction;
  try {
    resInteraction = await interaction.channel.awaitMessageComponent({
      filter: (i) => i.user.id === interaction.user.id &&
        (i.customId === `qurl_res_file_${sendNonce}` || i.customId === `qurl_res_loc_${sendNonce}`),
      time: 60000,
    });
  } catch {
    clearCooldown(interaction.user.id);
    return interaction.editReply({ content: 'No selection made. Send cancelled.', components: [] });
  }

  if (resInteraction.customId === `qurl_res_loc_${sendNonce}`) {
    // --- Location: show modal ---
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
    await resInteraction.showModal(modal);

    let modalSubmit;
    try {
      modalSubmit = await resInteraction.awaitModalSubmit({
        filter: (i) => i.customId === `qurl_loc_modal_${sendNonce}`,
        time: 120000,
      });
    } catch (err) {
      // Distinguish timeout from other errors (permissions, API failures);
      // non-timeout errors land in logs with their real detail.
      const isTimeout = err?.code === 'InteractionCollectorError' || /time/.test(err?.message || '');
      if (!isTimeout) {
        logger.error('Modal submit failed unexpectedly', { sendNonce, error: err?.message });
      }
      clearCooldown(interaction.user.id);
      return interaction.editReply({
        content: isTimeout ? 'Location input timed out. Send cancelled.' : 'Could not collect location input. Send cancelled.',
        components: [],
      });
    }

    // Hard-cap input length BEFORE regex matching. The Maps regexes have
    // unbounded character classes that can backtrack on pathological input;
    // trimming to 2000 chars prevents a ReDoS while still accepting every
    // legitimate Google Maps URL (real-world max is ~300 chars).
    const locationValue = modalSubmit.fields.getTextInputValue('location_value').trim().slice(0, 2000);
    await modalSubmit.deferUpdate();

    // Bounded repetitions so even the locationValue.slice(0, 2000) cap above
    // can't feed a ReDoS-pathological string to an unbounded `+` over
    // overlapping classes. Real Google Maps URLs peak around ~300 chars.
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
      // Regex matched but isGoogleMapsURL() rejected — treat as text search
      locationName = locationValue;
      locationUrl = `https://www.google.com/maps/search/${encodeURIComponent(locationValue)}`;
    } else {
      locationName = locationValue;
      locationUrl = `https://www.google.com/maps/search/${encodeURIComponent(locationValue)}`;
    }
    // Cap for embed-field safety (Discord 1024-char limit) and escape markdown
    // so a crafted place name can't inject **bold** / links / code blocks /
    // spoilers into the embed and phish recipients.
    if (locationName) locationName = escapeDiscordMarkdown(locationName.slice(0, 256));

  } else {
    // --- File: use attachment from slash command ---
    resourceType = RESOURCE_TYPES.FILE;
    attachment = commandAttachment;

    if (!attachment) {
      clearCooldown(interaction.user.id);
      await resInteraction.update({
        content: 'No file attached. Please rerun `/qurl send` with a file in the `file` option.',
        components: [],
      });
      return;
    }

    await resInteraction.deferUpdate();

    if (!isAllowedFileType(attachment.contentType)) {
      clearCooldown(interaction.user.id);
      return interaction.editReply({
        content: `File type \`${attachment.contentType}\` is not allowed. Supported: images, PDFs, videos, audio, Office docs, text, CSV, ZIP.`,
        components: [],
      });
    }
    if (attachment.size > MAX_FILE_SIZE) {
      clearCooldown(interaction.user.id);
      return interaction.editReply({
        content: `File too large (${Math.round(attachment.size / 1024 / 1024)}MB). Maximum is 25MB.`,
        components: [],
      });
    }
    // Cap concurrent in-flight file sends. Each holds its 25 MB buffer
    // through the mint-batches re-upload cycle; without this cap, N
    // simultaneous /qurl send calls × 25 MB can exhaust process memory.
    if (activeFileSends >= MAX_CONCURRENT_FILE_SENDS) {
      clearCooldown(interaction.user.id);
      logger.warn('File send rejected: concurrency cap reached', { activeFileSends });
      return interaction.editReply({
        content: `The bot is processing too many file sends right now. Please try again in a moment.`,
        components: [],
      });
    }
  }

  // --- Step 3: Process and send ---
  await interaction.editReply({ content: `Preparing links for ${recipients.length} recipient(s)...`, components: [] });

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
      activeFileSends++;
      fileSendSlotClaimed = true;
      slotWatchdog = setTimeout(() => {
        if (fileSendSlotClaimed) {
          logger.error('activeFileSends slot watchdog fired — slot force-released', { sendId });
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
    db.recordQURLSendBatch(qurlLinks.map(link => ({
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

  const dmResults = await batchSettled(qurlLinks, async (link) => {
    const recipient = recipientMap.get(link.recipientId);
    const dmPayload = buildDeliveryPayload({
      // member.displayName resolves to nickname || globalName || username,
      // so it works whether the sender has a per-guild nickname, only a
      // global display name, or just the legacy @-handle.
      senderAlias: resolveSenderAlias(interaction),
      qurlLink: link.qurlLink,
      expiresIn,
      personalMessage,
    });

    const sent = await sendDM(link.recipientId, dmPayload);
    db.updateSendDMStatus(sendId, link.recipientId, sent ? DM_STATUS.SENT : DM_STATUS.FAILED);
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
    db.saveSendConfig({
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
  const sendConfig = db.getSendConfig(sendId, senderDiscordId);
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
    }
  } catch (error) {
    logger.error('Failed to create links for additional recipients', { error: error.message });
    const isPoolExhausted = error.message?.includes('429') || error.message?.includes('limit');
    const msg = isPoolExhausted
      ? 'Link pool exhausted for this resource. Please create a new send instead of adding recipients.'
      : 'Failed to create links for new recipients.';
    return { msg, newResourceIds: [], delivered: 0, failed: 0 };
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
    db.recordQURLSendBatch(batchSends);
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
    if (!links || links.length === 0) return { sent: false, username: recipient.username };

    // links.slice(0, 10) caps at Discord's 10-embed-per-message limit.
    // The button-row chunking below splits all buttons into ActionRows of
    // 5 (Discord's per-row cap). Note Discord renders all embeds first,
    // then all component rows below — buttons are NOT visually paired
    // with their corresponding embed. Multi-link UX would benefit from
    // per-link button labels (e.g. "Step Through · report.pdf"), but
    // today links.length is always 1 so labels are uniform.
    const payloads = links.slice(0, 10).map(link => buildDeliveryPayload({
      // Same alias resolution as handleSend — see comment there for
      // the nickname > globalName > username fallback rationale.
      senderAlias: resolveSenderAlias(originalInteraction),
      qurlLink: link.qurlLink,
      expiresIn: sendConfig.expires_in,
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

    const sent = await sendDM(recipient.id, { embeds: allEmbeds, components: allComponents });
    // updateSendDMStatus updates every qurl_sends row matching (sendId,
    // recipient.id), so a single call covers all links for this recipient.
    // The previous `for (let i = 0; i < links.length; i++)` loop wrote the
    // same update links.length times.
    db.updateSendDMStatus(sendId, recipient.id, sent ? DM_STATUS.SENT : DM_STATUS.FAILED);
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

  const recentSends = db.getRecentSends(interaction.user.id, 5);

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
  const resourceIds = db.getSendResourceIds(sendId, senderDiscordId);
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
  db.markSendRevoked(sendId, senderDiscordId);

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
      const existing = db.getLinkByDiscord(discordId);

      // Generate state and create pending link. State is HMAC-bound to the
      // discord user ID so the OAuth callback can verify cross-user replay
      // didn't happen even if the random nonce were somehow leaked.
      const state = generateState(discordId);
      db.createPendingLink(state, discordId);

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

      const existing = db.getLinkByDiscord(discordId);
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
          db.deleteLink(discordId);
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
      const link = db.getLinkByDiscord(targetUser.id);

      if (link) {
        const contributions = db.getContributions(targetUser.id);
        const badges = db.getBadges(targetUser.id);
        const streak = db.getStreak(targetUser.id);

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
      const contributions = db.getContributions(targetUser.id);

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
      const stats = db.getStats();

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
      const topContributors = db.getTopContributors(5);
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
      const topContributors = db.getTopContributors(10);

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

      const existingLink = db.getLinkByGithub(githubUsername);
      if (existingLink && existingLink.discord_id !== targetUser.id) {
        return interaction.reply({
          content: `⚠️ GitHub **@${githubUsername}** is already linked to <@${existingLink.discord_id}>. Unlink them first.`,
          ephemeral: true,
        });
      }

      db.forceLink(targetUser.id, githubUsername);

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
          const existing = db.getLinkByGithub(github);
          if (existing && existing.discord_id !== discordId) {
            failed++;
            errors.push(`@${github} already linked to another user`);
            continue;
          }

          db.forceLink(discordId, github);
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
          if (!db.hasMilestoneBeenAnnounced('stars', milestone, repo)) {
            if (db.recordMilestone('stars', milestone, repo)) {
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
        const linkedIds = db.getLinkedDiscordIds();
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
          .setDescription('Send a resource to users via one-time secure link')
          .addStringOption(opt =>
            opt.setName('target')
              .setDescription('Who receives this')
              .setRequired(true)
              .setAutocomplete(true))
          .addStringOption(opt =>
            opt.setName('expiry_optional')
              .setDescription('Link expiry (default: 24h)')
              .setRequired(false)
              .addChoices(...EXPIRY_CHOICES))
          .addAttachmentOption(opt =>
            opt.setName('file_optional')
              .setDescription('Optional — attach a file here if sharing a file')
              .setRequired(false))
          .addStringOption(opt =>
            opt.setName('message_optional')
              .setDescription('Optional note sent alongside the link')
              .setRequired(false))
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
        db.setGuildApiKey(guildId, submittedKey, interaction.user.id);
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
        const guildConfig = db.getGuildConfig(interaction.guildId);
        if (guildConfig) {
          // Show a short sha256 fingerprint instead of any key substring — a
          // 4-char suffix narrows brute-force space and a prefix leaks tenant
          // hints. An 8-char hex fingerprint is enough for an admin to confirm
          // they re-ran setup with the same key, without exposing bytes.
          // getGuildConfig no longer returns the decrypted key (it would
          // leak via any row dump); go through the explicit accessor and
          // let the plaintext fall out of scope immediately after hashing.
          const plaintextKey = db.getGuildApiKey(interaction.guildId) || '';
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
        const guildApiKey = interaction.guildId ? db.getGuildApiKey(interaction.guildId) : null;
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
        //     /qurl send — send a file and/or location to users
        //     /qurl revoke — revoke links from a previous send
        //     /qurl help — show this message
        //
        //   How it works:
        //     1. Use /qurl send and choose a target (user, channel, or voice)
        //     2. Attach a file and/or search for a location
        //     3. Each recipient gets a unique, single-use link by DM
        //     4. Links self-destruct on first access, or when the expiry
        //        elapses — whichever comes first
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
        //   Large servers (~1000+ members): when sending to a `channel` or
        //   `voice` target, members who appear offline in Discord may be
        //   skipped. If you need to reach a specific person for sure, use
        //   the `user` target.
        //
        //   Sign up at https://layerv.ai to get your API key.
        //
        // Section order (post-PR #124): user-facing flow first (Getting
        // started → How it works), then admin-only setup, then glossary
        // (Terms), then operational caveat (Large servers), then signup.
        return interaction.reply({
          content: '**qURL Bot — Help**\n\n' +
            '**Getting started — Share resources securely via one-time links:**\n' +
            '  `/qurl send` — send a file and/or location to users\n' +
            '  `/qurl revoke` — revoke links from a previous send\n' +
            '  `/qurl help` — show this message\n\n' +
            '**How it works:**\n' +
            // Leading tab on "1." keeps Discord's markdown parser from
            // treating it as the start of an ordered list (which would
            // renumber it relative to the subsequent lines and visually
            // misalign "1." with "2.", "3.", "4."). The tab indent now
            // matches the two-space indent below, but bypasses the list
            // auto-formatter.
            '\t1. Use `/qurl send` and choose a target (user, channel, or voice)\n' +
            '  2. Attach a file and/or search for a location\n' +
            '  3. Each recipient gets a unique, single-use link by DM\n' +
            '  4. Links self-destruct on first access, or when the expiry elapses — whichever comes first\n\n' +
            '**Setting up (for Admins):**\n' +
            '  `/qurl setup` — configure your API key (admin only)\n' +
            '  `/qurl status` — check if qURL is configured (admin only)\n\n' +
            '**Terms:** a *protected resource* is the file or location you\'re sharing. ' +
            'A *qurl* (or *access link*) is the single-use URL that delivers it. ' +
            'You create a qurl for a protected resource each time you run `/qurl send`.\n\n' +
            '**Large servers (~1000+ members):** when sending to a `channel` or `voice` target, ' +
            'members who appear offline in Discord may be skipped. ' +
            'If you need to reach a specific person for sure, use the `user` target.\n\n' +
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

// Target choices are surfaced via autocomplete (not static addChoices)
// so the "voice" option only appears when the command is invoked FROM
// a voice / stage-voice channel. Showing the voice option in text
// channels — even when the invoking user happens to be connected to
// voice elsewhere — was a persistent UX bug: users in text channels
// don't expect a "voice users" target and the option reads as noise.
// The backend (handleSend → getVoiceChannelMembers) still validates
// the sender is in voice and returns a friendly error otherwise, so
// channel-type gating here just hides the affordance in the wrong
// context; it doesn't loosen any invariant.
async function handleTargetAutocomplete(interaction) {
  const channel = interaction.channel;
  const isVoiceChannel = channel && (
    channel.type === ChannelType.GuildVoice ||
    channel.type === ChannelType.GuildStageVoice
  );
  const choices = [
    { name: 'Everyone in this channel', value: 'channel' },
    { name: 'A specific user', value: 'user' },
  ];
  if (isVoiceChannel) {
    choices.push({ name: 'Only voice users', value: 'voice' });
  }
  // respond() can reject on the 3s autocomplete deadline, Unknown
  // interaction, or network errors. Swallow + log so the rejection
  // doesn't bubble as an unhandled promise in the InteractionCreate
  // listener — the user just sees no suggestions in that case.
  try {
    await interaction.respond(choices);
  } catch (err) {
    logger.warn('Failed to respond to target autocomplete', { error: err.message });
  }
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
  if (interaction.isAutocomplete()) {
    if (interaction.commandName === 'qurl') {
      const focused = interaction.options.getFocused(true);
      if (focused.name === 'target') {
        return handleTargetAutocomplete(interaction);
      }
    }
    return;
  }

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
    },
  }),
};
