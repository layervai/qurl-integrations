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
//   - User mentions:  <@123456> or <@!123456>   → user ID 123456
//   - Role mentions:  <@&987654>                → expand to current
//                                                 guild members of that role
// Mention extraction is regex-based — separators don't matter for
// the mentions themselves (`<@111><@222>` matches both). The
// separator class only affects the residue/invalid-token strip
// pass: whitespace, commas, semicolons, pipes, and slashes (lenient
// — covers free-form paste from contact lists / CSV-ish formats).
//
// What we reject (returned in `invalidTokens`):
//   - Channel mentions <#...>
//   - Custom emoji <:name:id>
//   - Bare plaintext (no `<@...>` wrapping) — there's no reliable way
//     to resolve "alice" to a user ID without an autocomplete round-trip,
//     and silently dropping it would mask user typos.
//
// `invalidTokens` are PRE-ESCAPED at the parser boundary:
//   - `@everyone` / `@here` are rewritten by inserting a zero-width
//     space (U+200B) after the `@` — visually identical but Discord's
//     tokenizer no longer recognizes the mass-mention shape. A caller
//     naively interpolating an invalid token into a user-visible
//     message (`` `Couldn't parse: ${invalidTokens.join(', ')}` ``)
//     cannot accidentally fan-out-ping the channel. Other shapes
//     that Discord would ping (`<@id>`, `<@&id>`) can't reach this
//     slot because they parse as valid mentions in the passes above.
//
// Output shape:
//   {
//     ids:           [<user_id>, ...],        // deduped, cap-applied
//     invalidTokens: [<raw_token>, ...],      // pre-escaped (see above)
//     cappedCount:   <number>,                // 0 if not capped; else
//                                             // `total_pre_cap - cap`
//   }
//
// `ids` is deduped, capped at `config.QURL_SEND_MAX_RECIPIENTS` (the
// same per-send cap the in-channel form enforces), and excludes the
// invoking user + bots (matches the form's self-send + bot rejection).
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
const USER_MENTION_RE = /<@!?(\d+)>/g;
const ROLE_MENTION_RE = /<@&(\d+)>/g;

// Hard cap on input length so an adversarial regex-blowup attack
// (10MB of `<@1>` repetitions) doesn't tie up the worker. Discord's
// slash-command STRING option max is 6000 chars (when not narrowed
// via setMaxLength); 4000 is a conservative budget that comfortably
// fits any legitimate recipient-list paste while leaving headroom
// before the API ceiling. 7b.2 should narrow the option's
// max_length to ≤4000 so this cap becomes defense-in-depth rather
// than the user-visible truncation point.
const MAX_INPUT_LENGTH = 4000;

// Resolve a list of mention tokens (raw "<@..>" / "<@&..>" / etc.) to
// a flat user-ID list with the cap + self/bot filters applied. Returns
// `{ ids, invalidTokens }`; never throws on parse errors — invalid
// tokens always land in `invalidTokens` for caller-side surfacing.
//
// `interaction` is the slash-command interaction; we need it for:
//   - interaction.user.id  → exclude the sender from recipients
//   - interaction.guild    → role-mention expansion via members.cache
//   - interaction.guild.members.cache.get(id).user.bot → bot filter
function parseRecipientMentions(raw, interaction) {
  // Defensive normalization: getString can return null when the option
  // is omitted, and the empty-string path is the same shape — return
  // an empty result rather than throwing. Caller branches on
  // `ids.length === 0` to decide whether to render the recipient picker.
  if (raw == null || typeof raw !== 'string') {
    return { ids: [], invalidTokens: [], cappedCount: 0 };
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
  if (truncated) {
    const lastOpen = input.lastIndexOf('<');
    const lastClose = input.lastIndexOf('>');
    if (lastOpen > lastClose) {
      input = input.slice(0, lastOpen);
    }
  }
  if (input.length === 0) {
    return { ids: [], invalidTokens: [], cappedCount: 0 };
  }

  // `seen` is the canonical post-filter unique-candidate set (every
  // ID we considered, regardless of cap). `ids` is the cap-bounded
  // subset returned to the caller. `cappedCount` is computed post-hoc
  // as `seen.size - ids.size` — accurate even when role expansion is
  // the source of the overflow.
  const seen = new Set();
  const ids = new Set();
  const invalidTokens = [];
  const senderId = interaction.user?.id;
  const guild = interaction.guild;
  const cap = config.QURL_SEND_MAX_RECIPIENTS;

  // Mark an ID as considered: dedupe via `seen`, add to `ids` only
  // while under cap. Role-contribution detection lives in the role
  // loop's `usable` counter — see comment there.
  function consider(id) {
    if (seen.has(id)) return;
    seen.add(id);
    if (ids.size < cap) ids.add(id);
  }

  for (const m of input.matchAll(USER_MENTION_RE)) {
    const id = m[1];
    if (id === senderId) continue;
    // Bot filter — best-effort via cache. A cache miss leaves the bot
    // in the result; downstream send-pipeline has its own bot check
    // (see existing form's `Cannot send to a bot` warning path) so
    // this layer being lossy is acceptable.
    const member = guild?.members?.cache?.get(id);
    if (member?.user?.bot) continue;
    consider(id);
  }

  // Role expansion: missing role / empty role / DM-context guild lands
  // the raw role token in `invalidTokens` so the caller can surface
  // "couldn't resolve @role" rather than silently produce a smaller
  // recipient list.
  for (const m of input.matchAll(ROLE_MENTION_RE)) {
    const roleId = m[1];
    const role = guild?.roles?.cache?.get(roleId);
    if (!role) {
      invalidTokens.push(m[0]);
      continue;
    }
    // role.members is a Collection<Snowflake, GuildMember>. Empty for
    // a role with no current members in the cache; same treatment.
    const members = role.members;
    if (!members || members.size === 0) {
      invalidTokens.push(m[0]);
      continue;
    }
    // `usable` counts post-filter members the role exposed (whether
    // or not they were new to `seen`). Used to detect "all filtered"
    // vs "role contributed but we'd already counted them" — dedupe
    // is still a contribution.
    let usable = 0;
    for (const [memberId, member] of members) {
      if (memberId === senderId) continue;
      if (member?.user?.bot) continue;
      usable++;
      consider(memberId);
    }
    if (usable === 0) {
      // Role had members but they were all filtered (sender + bots).
      // Surface as "no usable members" rather than silently no-op so
      // the caller can tell the user "the role only contains you / bots."
      invalidTokens.push(m[0]);
    }
  }

  // Detect non-mention residue (channel mentions, custom emoji, bare
  // names) by stripping valid mentions and surfacing what's left.
  // Intentionally lossy on subtle parsing — the goal is "tell the user
  // we didn't understand THIS bit", not a perfect tokenizer.
  //
  // Separator class includes `;`, `|`, `/` in addition to whitespace
  // and `,` — these are common when pasting from contact lists or
  // CSV-ish formats. Without them the user gets `;` and `|` echoed
  // back as "invalid tokens."
  const stripped = input
    .replace(USER_MENTION_RE, ' ')
    .replace(ROLE_MENTION_RE, ' ');
  const leftover = stripped.split(/[\s,;|/]+/).filter(Boolean);
  for (const tok of leftover) {
    // Pre-escape `@everyone` / `@here` so a caller interpolating
    // `invalidTokens` into a user-visible message can't accidentally
    // fan-out-ping the channel. Insert a zero-width-space after `@`
    // — the rendered glyph is identical, but Discord's tokenizer
    // sees a different word and won't trigger the mass mention.
    // Use a regex (not exact-match) because `@everyone!`,
    // `@everyone:`, `@everyone.fix` etc. are single tokens after
    // the strip pass (residue split class doesn't include
    // punctuation) and would otherwise slip through. Replace all
    // occurrences so a token like `here@everyone` is fully fenced.
    invalidTokens.push(tok.replace(/@(everyone|here)/g, '@\u200b$1'));
  }

  // Cap was enforced inline during the two passes (`ids` never
  // exceeds `cap`; over-cap candidates live in `seen` but not `ids`).
  // The back-half re-enforces the cap too; surfacing `cappedCount`
  // here lets the caller say "I kept N of M — drop these or split"
  // before they hit Send rather than waiting for the back-half error.
  const finalIds = [...ids];
  const cappedCount = seen.size - ids.size;
  if (cappedCount > 0) {
    // Massive over-cap (>2× the limit) signals user confusion —
    // probably pasted a list they didn't trim. Surface at warn so
    // oncall sees the pattern. Modest overshoot is normal-ish
    // (typed too many) and stays at debug.
    const logFn = seen.size > cap * 2 ? logger.warn : logger.debug;
    logFn('recipient-parser: capping recipient list at QURL_SEND_MAX_RECIPIENTS', {
      raw_count: seen.size, cap,
    });
  }

  return { ids: finalIds, invalidTokens, cappedCount };
}

module.exports = {
  parseRecipientMentions,
  MAX_INPUT_LENGTH,
};
