// recipient-parser — extracts user/role mentions from the `recipients:`
// slash-command option string and resolves them to a flat user-ID list.
//
// `/qurl file` and `/qurl map` expose `recipients:` as a STRING option
// rather than a list of mentionables because Discord's slash-command
// `mentionable` option type is single-value — a power-user one-shot
// path needs a free-form text that can carry many mentions, role
// mentions, or a mix. The same string also has to survive the round-
// trip through interaction.options.getString (no parsing — Discord
// hands us the raw "<@123> <@456> <@&789>" text).
//
// What we accept:
//   - User mentions:    <@123456> or <@!123456>   → user ID 123456
//   - Role mentions:    <@&987654>                → expand to current
//                                                   guild members of that role
//   - Channel mentions: <#456789>                 → only voice / stage-voice
//                                                   channels the sender can
//                                                   see (ViewChannel-gated);
//                                                   expand to the voice-
//                                                   connected non-bot member
//                                                   set via `channel.members`.
//                                                   Non-voice channels and
//                                                   non-visible voice
//                                                   channels both land in
//                                                   `invalidTokens` so the
//                                                   caller can surface
//                                                   "couldn't expand
//                                                   #channel" rather than
//                                                   silently dropping
//                                                   (or, worse, leaking
//                                                   members of a private
//                                                   channel whose snowflake
//                                                   the sender knows).
// Mention extraction is regex-based — separators don't matter for
// the mentions themselves (`<@111><@222>` matches both). The
// separator class only affects the residue/invalid-token strip
// pass: whitespace, commas, semicolons, pipes, and slashes (lenient
// — covers free-form paste from contact lists / CSV-ish formats).
//
// What we reject (returned in `invalidTokens`):
//   - Channel mentions <#...> for non-voice channels (see above).
//   - Custom emoji <:name:id>
//   - Bare plaintext (no `<@...>` wrapping) — there's no reliable way
//     to resolve "alice" to a user ID without an autocomplete round-trip,
//     and silently dropping it would mask user typos.
//
// SECURITY: `invalidTokens` are PRE-ESCAPED at the parser boundary:
//   - `@everyone` / `@here` are rewritten by inserting a zero-width
//     space (U+200B) after the `@` — visually identical but Discord's
//     tokenizer no longer recognizes the mass-mention shape. A caller
//     naively interpolating an invalid token into a user-visible
//     message (`` `Couldn't parse: ${invalidTokens.join(', ')}` ``)
//     cannot accidentally fan-out-ping the channel. Other shapes
//     that Discord would ping (`<@id>`, `<@&id>`) can't reach this
//     slot because they parse as valid mentions in the passes above.
//
//     Note: standalone `@everyone` tokens are intercepted upstream by
//     the gated detect-and-strip pass (`allowMassMention` opt — see
//     parseRecipientMentions docstring below). The defuse here is the
//     fallback for `@here`, `@Everyone` (inert case), and embedded
//     forms like `here@everyone` that escape the gated path's word-
//     boundary. Both layers preserve the "caller-can-interpolate-safely"
//     contract above.
//
// SECURITY: `invalidTokens` ARE NOT escaped against Discord markdown — a pasted
// `[link](https://evil)`, `||spoiler||`, or backtick-fenced content
// will render with full markdown semantics if a caller interpolates
// the token bare into a message. The caller (7b.2's error renderer)
// MUST wrap `invalidTokens` in a code block, or escape `[`, `]`,
// `` ` ``, `|`, `*`, `_` before display. The mass-mention escape is
// special-cased because it's the only shape with off-channel side
// effects (a real ping to thousands of users); other markdown is
// "ugly rendering" not "security."
//
// Output shape:
//   {
//     ids:                [<user_id>, ...],   // deduped, cap-applied
//     invalidTokens:      [<raw_token>, ...], // pre-escaped (see above)
//     cappedCount:        <number>,           // 0 if not capped; else
//                                             // `total_pre_cap - cap`
//     massMentionDenied:  <boolean>,          // true when @everyone
//                                             // appeared but the caller
//                                             // denied via the
//                                             // `allowMassMention` opt
//   }
//
// `ids` is deduped, capped at `config.QURL_SEND_MAX_RECIPIENTS` (the
// same per-send cap the in-channel form enforces), and excludes bots
// (matches the form's bot rejection). The invoking user IS allowed as
// a recipient — self-send is a supported use case (test the link
// before forwarding, cross-device handoff, etc.).
//
// Role expansion uses interaction.guild.members.cache via discord.js's
// `Role.members` getter, which filters the guild's member cache for
// the role. PARTIAL-CACHE BLIND SPOT: in large guilds running without
// `GUILD_MEMBERS` intent (or before chunking), `role.members.size`
// reflects only the currently-cached subset, not the role's true
// population. We cannot distinguish "small role" from "large role,
// partial cache." Cold-cache / DM-context degrades safely (token
// lands in `invalidTokens`), but a partial-cache silent under-resolve
// is harder to surface — 7b.2 should consider logging `role.memberCount`
// vs `role.members.size` when they diverge, and the user-facing copy
// should say "expanded N members" so users notice if a role appears
// to expand to far fewer recipients than they expect.

const config = require('./config');
const logger = require('./logger');

// Capture group is the snowflake ID. Discord IDs are 17-20 digit
// integers; the regex is intentionally loose on length (matches any
// 1+ digit run) so a future ID-width change doesn't silently drop
// real IDs. The format-validation that matters is "is this actually
// a user/role Discord knows about" — fetched lazily on resolution.
// Module-scope `/g` regexes are safe to share across calls: `matchAll`
// clones the regex per-iteration, and `String.prototype.replace`
// doesn't mutate `lastIndex`. (The `lastIndex` footgun applies only
// to `RegExp.prototype.exec` / `RegExp.prototype.test` on the same
// regex instance.)
const USER_MENTION_RE = /<@!?(\d+)>/g;
const ROLE_MENTION_RE = /<@&(\d+)>/g;
// Matches `@everyone` as a standalone token (Unicode word boundaries
// on both sides). Intentionally narrower than the U+200B defuse regex
// below — here we're matching for INCLUSION, so we want only real
// Discord mass-mention tokens, not substrings like `@everyonefoo`
// that would false-positive. Case-sensitive because Discord's mass-
// mention parser is itself lowercase-only (`@Everyone` is inert).
// `\p{L}\p{N}_` (Unicode letters/numbers + underscore) instead of
// ASCII-only `[A-Za-z0-9_]` so `@everyoneé` / `@everyonefoö` don't
// false-match the way an ASCII boundary would. Discord's tokenizer
// is itself Unicode-aware here; the `/u` flag aligns with that.
// `@here` is NOT matched — would need GUILD_PRESENCES intent (we
// only run GuildMembers) to filter online members, and treating it
// as `@everyone` would be deceptive. Falls through to the invalidTokens
// defuse path as before; revisit if the bot ever adds the intent.
const EVERYONE_TOKEN_RE = /(?<![\p{L}\p{N}_])@everyone(?![\p{L}\p{N}_])/gu;

// Channel mention shape: `<#123>`. Discord also emits `<#!123>` for
// some legacy contexts, but the slash-input field never produces the
// `!` form for channels (only for users), so the regex stays strict.
const CHANNEL_MENTION_RE = /<#(\d+)>/g;

// discord.js GatewayIntentBits.GuildVoice / GuildStageVoice enum
// values pinned here (2 / 13) so the parser doesn't have to require
// `discord.js` for one constant pair, and so unit tests can build a
// `channel` mock without instantiating discord.js's ChannelType. A
// discord.js bump that renumbers these would break voice expansion
// silently; the `voice-channel type constants` describe block in
// `recipient-parser.test.js` pins the contract.
const VOICE_CHANNEL_TYPE = 2;
const STAGE_VOICE_CHANNEL_TYPE = 13;

// discord.js PermissionFlagsBits.ViewChannel pinned numerically (bit
// 10 = 1 << 10) for the same reason as the channel-type constants:
// no discord.js require in the parser, plus a test spec pins the
// value against future discord.js renumbers. discord.js represents
// permissions as BigInt; PermissionsBitField.has accepts either a
// BigInt or a number, so the numeric literal works for both prod
// (real PermissionsBitField) and tests (Map-shaped mock).
const VIEW_CHANNEL_PERMISSION = 1n << 10n;

// Hard cap on input length so an adversarial regex-blowup attack
// (10MB of `<@1>` repetitions) doesn't tie up the worker. Discord's
// slash-command STRING option max is 6000 chars (when not narrowed
// via setMaxLength).
//
// 7b.2 narrows the slash option to MAX_SLASH_OPTION_LENGTH (2000)
// — well below this parser cap (4000) — so:
//   * the parser cap is genuine defense-in-depth (only reachable
//     via a forged interaction OR a future non-slash caller), and
//   * the partial-mention trim below is unreachable from `/qurl
//     file` / `/qurl map`'s slash path.
// Legitimate recipient lists peak around 25 mentions × ~24 chars =
// ~600 chars; 2000 leaves headroom for role-mention expansion and
// pasted formatting without ever hitting the parser cap.
const MAX_INPUT_LENGTH = 4000;

// Max length applied to `/qurl file` / `/qurl map` `recipients:`
// slash options. Discord enforces this server-side, so anything past
// it never reaches the parser — the parser's MAX_INPUT_LENGTH cap
// (above) is genuine defense-in-depth, only reachable via a forged
// interaction or a future non-slash caller.
//
// Picked at half of MAX_INPUT_LENGTH so the headroom relationship is
// load-bearing: a future bump to MAX_INPUT_LENGTH (e.g. for a larger
// guild's role-expansion needs) preserves the 2× gap automatically.
// Both /qurl file and /qurl map builders read this; 7b.3+ should
// continue to honor the gap.
const SLASH_OPTION_HEADROOM_DIVISOR = 2;
const MAX_SLASH_OPTION_LENGTH = Math.floor(MAX_INPUT_LENGTH / SLASH_OPTION_HEADROOM_DIVISOR);

// Per-token cap on entries in `invalidTokens`. Discord's embed total
// is 4096 chars; a single ~4000-char garbage token (one giant string
// with no separators) would blow that budget. Truncating at the
// parser boundary means the caller's error renderer can interpolate
// `invalidTokens` without further trimming. Applied to residue
// tokens only — `<@&id>` role tokens are bounded by Discord ID
// width and don't need it.
const MAX_INVALID_TOKEN_LENGTH = 256;

// Cap-overshoot logging threshold: above this multiple of the cap,
// escalate the log level from `debug` to `warn`. The signal is "user
// pasted an untrimmed list" (vs the more common "typed too many"),
// which oncall benefits from seeing as a pattern.
const MASSIVE_OVERSHOOT_MULTIPLIER = 2;

// Single-source the GuildMember-shape bot check used by every
// expansion path (direct user mention, role expansion, @everyone
// expansion). A future discord.js rename of `.user.bot` only needs
// to touch one line.
function isBotMember(member) {
  return member?.user?.bot === true;
}

// Resolve a list of mention tokens (raw "<@..>" / "<@&..>" / `@everyone`)
// to a flat user-ID list with the cap + bot filter applied. Returns
// `{ ids, invalidTokens, cappedCount, massMentionDenied }`; never
// throws on parse errors — invalid tokens always land in
// `invalidTokens` for caller-side surfacing. Sender is NOT filtered —
// self-send is supported; the confirm-card renderer surfaces a neutral
// notice when sender appears in `ids`.
//
// `@everyone` is a privileged mention shape. The caller decides whether
// to honor it via `opts.allowMassMention` (typically gated behind
// Discord's MENTION_EVERYONE permission, checked on the slash-command
// interaction). When the token appears in input:
//   - allowed → expanded to all non-bot guild members in the cache.
//   - denied  → `massMentionDenied: true` in the result so the caller
//               can surface "you don't have permission to @everyone".
// Either way the token is stripped from `invalidTokens` to avoid
// double-surfacing.
//
// `interaction` is the slash-command interaction; we need it for:
//   - interaction.guild               → role-mention expansion via
//                                       roles.cache + `@everyone`
//                                       expansion via members.cache
//   - interaction.guild.members.cache → bot filter (per-id `.user.bot`
//                                       lookup) + `@everyone` member
//                                       enumeration
function parseRecipientMentions(raw, interaction, opts = {}) {
  const allowMassMention = opts.allowMassMention === true;
  // Defensive normalization: getString can return null when the option
  // is omitted, and the empty-string path is the same shape — return
  // an empty result rather than throwing. Caller branches on
  // `ids.length === 0` to decide whether to render the recipient picker.
  if (raw == null || typeof raw !== 'string') {
    return { ids: [], invalidTokens: [], cappedCount: 0, massMentionDenied: false };
  }
  // Mirror the `raw` guard for `interaction` so a null/undefined
  // caller-bug surfaces as an empty result instead of a TypeError
  // at the `interaction.guild` deref below. The back-half has
  // a clearer crash site for "no caller context"; the parser
  // doesn't gain anything by failing here.
  if (interaction == null) {
    return { ids: [], invalidTokens: [], cappedCount: 0, massMentionDenied: false };
  }
  // Length-cap BEFORE regex matching to keep the global-flag iteration
  // bounded under adversarial input. The /g flag scans linearly but
  // pathological repetitions still allocate per-match strings. This
  // cap MUST precede `matchAll` — a refactor that flipped to
  // "validate then truncate" would silently regress the ReDoS guard.
  const truncated = raw.length > MAX_INPUT_LENGTH;
  let input = truncated ? raw.slice(0, MAX_INPUT_LENGTH) : raw;
  // If the cut lands inside a `<...>` token, drop the trailing partial
  // so the strip-pass doesn't surface a manufactured "invalid token"
  // the user didn't actually mistype. Gate strictly on `truncated` —
  // a naturally-MAX-length input with a stray `<` mid-string is
  // legitimate content that the strip-pass should surface as-is.
  //
  // The `lastOpen > lastClose` heuristic is intentionally one-sided:
  // false-positive-resistant on residue (won't manufacture a fake
  // token from a clean cut), lossy by design on legitimate content
  // past the cut (e.g. `<literal>` in a paste). Acceptable since
  // truncation only fires past MAX, and 7b.2 should narrow the
  // slash-option's max_length so this branch is defense-in-depth
  // rather than a user-visible loss.
  if (truncated) {
    const lastOpen = input.lastIndexOf('<');
    const lastClose = input.lastIndexOf('>');
    if (lastOpen > lastClose) {
      input = input.slice(0, lastOpen);
    }
  }
  if (input.length === 0) {
    return { ids: [], invalidTokens: [], cappedCount: 0, massMentionDenied: false };
  }

  // `seen` is the canonical post-filter unique-candidate set (every
  // ID we considered, regardless of cap). `ids` is the cap-bounded
  // subset returned to the caller. `cappedCount` is computed post-hoc
  // as `seen.size - ids.size` — accurate even when role expansion is
  // the source of the overflow.
  const seen = new Set();
  const ids = new Set();
  const invalidTokens = [];
  const guild = interaction.guild;
  // `cap > 0` is guaranteed by the `intEnv(..., { minPositive: true })`
  // validator on QURL_SEND_MAX_RECIPIENTS in config.js — if the env
  // override ever flipped to ≤ 0, that validator would crash boot,
  // not silently produce an empty result here.
  const cap = config.QURL_SEND_MAX_RECIPIENTS;

  // Mark an ID as considered: dedupe via `seen`, add to `ids` only
  // while under cap. Role-contribution detection lives in the role
  // loop's `usable` counter — see comment there.
  function consider(id) {
    if (seen.has(id)) return;
    seen.add(id);
    if (ids.size < cap) ids.add(id);
  }

  // Detect-and-strip `@everyone` BEFORE the mention passes so the
  // token doesn't leak into the residue (which would U+200B-defuse
  // it into `invalidTokens` regardless of allow/deny). The actual
  // expansion runs LAST (after user + role mentions), so explicit
  // mentions get cap priority. Detect-via-replace (compare strings)
  // instead of `.test()` so the shared module-scope `/g` regex's
  // `lastIndex` doesn't carry state across calls.
  let massMentionDenied = false;
  let everyonePresent = false;
  const everyoneStripped = input.replace(EVERYONE_TOKEN_RE, ' ');
  if (everyoneStripped !== input) {
    input = everyoneStripped;
    everyonePresent = true;
    if (!allowMassMention) {
      // Denied path — surface so the caller can warn ("you don't
      // have MENTION_EVERYONE permission"). Distinct signal from
      // invalidTokens so the caller can emit the permission copy
      // rather than the generic "couldn't parse" copy. The allowed-
      // path expansion happens below, after explicit mentions.
      massMentionDenied = true;
    }
  }

  for (const m of input.matchAll(USER_MENTION_RE)) {
    const id = m[1];
    // Bot filter — best-effort via cache. A cache miss (cold cache,
    // partial cache without GUILD_MEMBERS intent, or DM-context with
    // `guild === undefined`) leaves the bot in the result; downstream
    // send-pipeline has its own bot check (see existing form's
    // `Cannot send to a bot` warning path) so this layer being lossy
    // is acceptable. 7b.2 wiring should NOT assume the parser
    // filtered bots.
    const member = guild?.members?.cache?.get(id);
    if (isBotMember(member)) continue;
    consider(id);
  }

  // Role expansion: missing role / empty role / DM-context guild
  // / all-members-filtered all land the raw role token in
  // `invalidTokens` so the caller can surface "couldn't resolve
  // @role" rather than silently produce a smaller recipient list.
  // Self-send via role: if the sender is a member of the mentioned
  // role, they contribute to the recipient list along with the
  // role's other (non-bot) members. The confirm card's "Send includes
  // you." notice surfaces this so users aren't surprised when an
  // `@team` mention includes themselves.
  // NOTE: these three states are CONFLATED in the current shape —
  // 7b.2 may want to distinguish "role doesn't exist" / "role has
  // no members" / "all members are bots" via separate fields
  // (e.g. `emptyRoles`, or check `role.memberCount > 0 && members.size
  // === 0` for the partial-cache case). For now, conflation is
  // acceptable because the user-visible copy ("couldn't expand @role")
  // works for all three. NOTE on perf: discord.js's `Role.members`
  // getter filters the full guild member cache per call, so N role
  // mentions = N O(guild_size) scans. Bounded by the 4000-char input
  // cap and Discord's slash-option max.
  // Per-kind invalidTokens dedupe sets. Sharing the dedupe helper
  // across role + channel error paths means `<@&999> <@&999>` and
  // `<#999> <#999>` each yield one invalidTokens entry, not two —
  // the strip-pass's residue-tokens already dedupe naturally via
  // input grouping, and this restores the same property for the
  // expansion paths.
  const invalidRoleIds = new Set();
  function pushInvalidIfNew(seenIds, id, rawToken) {
    if (seenIds.has(id)) return;
    seenIds.add(id);
    invalidTokens.push(rawToken);
  }
  for (const m of input.matchAll(ROLE_MENTION_RE)) {
    const roleId = m[1];
    const role = guild?.roles?.cache?.get(roleId);
    if (!role) {
      pushInvalidIfNew(invalidRoleIds, roleId, m[0]);
      continue;
    }
    // role.members is a Collection<Snowflake, GuildMember>. Empty for
    // a role with no current members in the cache; treated like an
    // unknown role (lands in invalidTokens so the caller can surface
    // "couldn't expand @role").
    const members = role.members;
    if (!members || members.size === 0) {
      pushInvalidIfNew(invalidRoleIds, roleId, m[0]);
      continue;
    }
    // `usable` counts members that passed the bot filter, INCLUDING
    // ones already in `seen` (i.e. a role whose every member was
    // already direct-mentioned still contributes — dedupe counts).
    // Drives ONLY the "no usable members" → invalidTokens branch
    // below: empty role / bots-only role. Critically, `usable++` runs
    // BEFORE the seen-check inside `consider`, so the "fully-duplicate
    // role isn't useless" test catches a future refactor reversing
    // that order.
    let usable = 0;
    for (const [memberId, roleMember] of members) {
      if (isBotMember(roleMember)) continue;
      usable++;
      consider(memberId);
    }
    if (usable === 0) {
      // Role had members but they were all filtered out as bots.
      // Surface as "no usable members" rather than silently no-op so
      // the caller can tell the user "the role only contains bots."
      pushInvalidIfNew(invalidRoleIds, roleId, m[0]);
    }
  }

  // Channel expansion: voice / stage-voice channels expand to their
  // currently-connected non-bot member set. Runs BEFORE @everyone so
  // explicit channel mentions claim cap slots first. `channel.members`
  // reads the voice-state cache, which is only populated when the
  // `GuildVoiceStates` gateway intent is declared (pinned by the boot
  // canary in discord.js).
  const invalidChannelIds = new Set();
  for (const m of input.matchAll(CHANNEL_MENTION_RE)) {
    const channelId = m[1];
    const channel = guild?.channels?.cache?.get(channelId);
    if (!channel) {
      pushInvalidIfNew(invalidChannelIds, channelId, m[0]);
      continue;
    }
    const isVoice = channel.type === VOICE_CHANNEL_TYPE
      || channel.type === STAGE_VOICE_CHANNEL_TYPE;
    if (!isVoice) {
      // Non-voice channel mention — reject. We intentionally do NOT
      // restore the legacy "Everyone in this text channel" behavior
      // (PR #174 fixed the @everyone-on-default-server expansion bug
      // by removing it). Voice membership is unambiguously
      // "voice-connected"; text-channel membership is permission-
      // bound and varies across server topologies.
      pushInvalidIfNew(invalidChannelIds, channelId, m[0]);
      continue;
    }
    // ViewChannel gate — the bot's channels.cache holds every channel
    // it has visibility into, so without this check a user who
    // discovers a private voice channel's snowflake (audit logs,
    // mod-tool exports, leaked URLs) could DM-blast its connected
    // members via `/qurl file recipients:<#hidden-voice-id>` even
    // when they themselves can't see the channel in the client.
    // Fail-closed: if `permissionsFor` is missing (degraded test
    // mock, future discord.js shape change) or returns falsy, we
    // reject the mention. The button-driven voice-everyone path
    // doesn't need this gate — invoking the slash command from
    // inside a voice channel intrinsically proves visibility.
    const viewerPerms = channel.permissionsFor?.(interaction.member);
    if (!viewerPerms || !viewerPerms.has(VIEW_CHANNEL_PERMISSION)) {
      pushInvalidIfNew(invalidChannelIds, channelId, m[0]);
      continue;
    }
    // channel.members for voice channels is a Collection<Snowflake,
    // GuildMember> of voice-connected members. Empty when no one is
    // connected — treat like an unknown channel so the caller can
    // surface "no one is in #channel" rather than silently produce a
    // smaller recipient list. Bot filter mirrors the user-mention,
    // role-expansion, and @everyone passes via isBotMember.
    const members = channel.members;
    if (!members || members.size === 0) {
      pushInvalidIfNew(invalidChannelIds, channelId, m[0]);
      continue;
    }
    // Walks every member regardless of cap — `consider()` caps `ids`
    // but `seen` grows unbounded. Symmetric with the @everyone loop
    // below: in both cases, iteration is bounded by Discord's channel
    // capacity (99 for voice, ~10k for stage), and the cost is
    // dominated by `partitionRecipients` + transitionFlow downstream.
    let usable = 0;
    for (const [memberId, channelMember] of members) {
      if (isBotMember(channelMember)) continue;
      usable++;
      consider(memberId);
    }
    if (usable === 0) {
      pushInvalidIfNew(invalidChannelIds, channelId, m[0]);
    }
  }

  // @everyone expansion runs LAST so explicit mentions (user / role /
  // channel) get cap priority. If the user typed
  // `@everyone <@uncachedUser>`, the direct mention claims a cap slot
  // first, and @everyone fills the remainder. Reversing this order
  // silently drops explicit mentions when the cache is partial.
  if (everyonePresent && allowMassMention) {
    const everyoneMembers = guild?.members?.cache;
    if (everyoneMembers) {
      // PARTIAL-CACHE BLIND SPOT: same caveat as role expansion
      // above — in large guilds without a recent GUILD_MEMBERS
      // chunk, the cache reflects only the currently-loaded subset,
      // not the guild's true member count. Acceptable v1; in
      // realistic guilds (where bots are <1% of members) the cap
      // (QURL_SEND_MAX_RECIPIENTS) bounds the iteration via the
      // `break` below. The pathological all-bots case still walks
      // the entire cache — bounded only by cache size, not the cap.
      //
      // ORDERING is cache-insertion-order (discord.js Collection
      // is a Map): when the cap short-circuits the loop, members
      // win the remaining slots BY CACHE INSERTION ORDER — not
      // alphabetical, role-ordered, or recently-active. Users may
      // see a different subset across invocations as the cache
      // churns. v2 picker (#324 — MentionableSelectMenu) shifts the
      // selection burden onto the user and removes this ambiguity.
      //
      // ITERATION ASSUMES NO CONCURRENT MUTATION: discord.js itself
      // doesn't guarantee iteration safety for Collections under
      // gateway-driven mutation (member join/leave events). This
      // parser runs synchronously inside the slash-command handler
      // — no awaits between the start of this loop and its `break`
      // — so the cache snapshot is effectively frozen for the loop
      // body. A future refactor that introduced an `await` here
      // would break that assumption.
      for (const [memberId, member] of everyoneMembers) {
        // Short-circuit once we've filled the cap. We accept the
        // tradeoff of inaccurate cappedCount (no past-cap members
        // get added to `seen`) over scanning a 10k-member cache.
        // The cap message still surfaces from any explicit mentions
        // that overflowed.
        if (ids.size >= cap) break;
        if (isBotMember(member)) continue;
        consider(memberId);
      }
    }
  }

  // Detect non-mention residue (custom emoji, bare names, unknown
  // bracketed tokens) by stripping valid mentions and surfacing what's
  // left. Intentionally lossy on subtle parsing — the goal is "tell the
  // user we didn't understand THIS bit", not a perfect tokenizer.
  //
  // Separator class includes `;`, `|`, `/` in addition to whitespace
  // and `,` — these are common when pasting from contact lists or
  // CSV-ish formats. Without them the user gets `;` and `|` echoed
  // back as "invalid tokens."
  //
  // CHANNEL_MENTION_RE is stripped here even when the channel resolved
  // to an invalidTokens entry above — the channel-expansion pass
  // already surfaced it; the residue pass would otherwise double-
  // report the same `<#id>` as a leftover token.
  const stripped = input
    .replace(USER_MENTION_RE, ' ')
    .replace(ROLE_MENTION_RE, ' ')
    .replace(CHANNEL_MENTION_RE, ' ');
  const leftover = stripped.split(/[\s,;|/]+/).filter(Boolean);
  const residueSeen = new Set();
  for (const tok of leftover) {
    // Pre-escape `@everyone` / `@here` so a caller interpolating
    // `invalidTokens` into a user-visible message can't accidentally
    // fan-out-ping the channel. Insert a zero-width-space after `@`
    // — the rendered glyph is identical, but Discord's tokenizer
    // sees a different word and won't trigger the mass mention.
    // Standalone `@everyone` is intercepted by the gated detect-and-
    // strip pass above, so the cases this defuse still catches are
    // `@here`, `@Everyone` (case-mismatched / inert), and embedded
    // forms like `here@everyone` that escape the gated path's word-
    // boundary. Use a regex (not exact-match) because shapes like
    // `@here:`, `here@everyone` survive the strip pass as single
    // tokens (residue split class doesn't include punctuation) and
    // would otherwise slip through. Replace all occurrences so a
    // token like `here@everyone` is fully fenced.
    //
    // INTENTIONALLY case-sensitive (no `/i` flag) \u2014 Discord's mass-
    // mention parser is itself lowercase-only, so `@Everyone` is
    // already inert. Widening would needlessly mangle legitimate
    // `@Everyone` paste artifacts. The `@Everyone-not-escaped`
    // test pins this invariant.
    let escaped = tok.replace(/@(everyone|here)/g, '@\u200b$1');
    // Per-token length cap (see MAX_INVALID_TOKEN_LENGTH doc above).
    // Truncation runs AFTER the escape; defense-in-depth re-escape
    // below catches the edge case where the cut leaves an `@everyone`
    // suffix-fragment that's somehow still parseable (e.g. if the
    // escape ever changes to a positional pattern). Cheap pass.
    if (escaped.length > MAX_INVALID_TOKEN_LENGTH) {
      escaped = `${escaped.slice(0, MAX_INVALID_TOKEN_LENGTH)}\u2026`;
      escaped = escaped.replace(/@(everyone|here)/g, '@\u200b$1');
    }
    // Dedupe residue tokens by the post-escape rendered form so
    // `<#456> <#456>` or `alice alice` produces ONE entry, not two.
    // Symmetric with the role/channel-error dedup paths
    // (pushInvalidIfNew above) \u2014 a caller's "couldn't parse: X, X"
    // embed is hostile.
    if (residueSeen.has(escaped)) continue;
    residueSeen.add(escaped);
    invalidTokens.push(escaped);
  }

  // Cap was enforced inline during the two passes (`ids` never
  // exceeds `cap`; over-cap candidates live in `seen` but not `ids`).
  // The back-half re-enforces the cap too; surfacing `cappedCount`
  // here lets the caller say "I kept N of M — drop these or split"
  // before they hit Send rather than waiting for the back-half error.
  const finalIds = [...ids];
  const cappedCount = seen.size - ids.size;
  if (cappedCount > 0) {
    const logFn = seen.size > cap * MASSIVE_OVERSHOOT_MULTIPLIER
      ? logger.warn
      : logger.debug;
    logFn('recipient-parser: capping recipient list at QURL_SEND_MAX_RECIPIENTS', {
      uniqueCount: seen.size, cap, cappedCount,
    });
  }

  return { ids: finalIds, invalidTokens, cappedCount, massMentionDenied };
}

// `isVoiceChannelType` is the same voice/stage-voice predicate the
// channel-mention expander uses, exported so the confirm-card renderer
// can detect "this slash command was invoked from a voice channel"
// without dragging in the discord.js ChannelType enum. Keeping the
// predicate parser-owned ensures both the `<#voice>` expansion and
// the confirm-card button branch on the same set of channel types.
function isVoiceChannelType(type) {
  return type === VOICE_CHANNEL_TYPE || type === STAGE_VOICE_CHANNEL_TYPE;
}

module.exports = {
  parseRecipientMentions,
  isVoiceChannelType,
  isBotMember,
  MAX_INPUT_LENGTH,
  MAX_INVALID_TOKEN_LENGTH,
  MAX_SLASH_OPTION_LENGTH,
  VOICE_CHANNEL_TYPE,
  STAGE_VOICE_CHANNEL_TYPE,
  VIEW_CHANNEL_PERMISSION,
};
