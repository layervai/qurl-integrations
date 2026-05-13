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
//   - Whitespace + commas as separators (lenient — the user typed it)
//
// What we reject (returned in `invalidTokens`):
//   - Channel mentions <#...>
//   - Custom emoji <:name:id>
//   - Bare plaintext (no `<@...>` wrapping) — there's no reliable way
//     to resolve "alice" to a user ID without an autocomplete round-trip,
//     and silently dropping it would mask user typos.
//
// Output shape:
//   { ids: [<user_id>, ...], invalidTokens: [<raw_token>, ...] }
//
// `ids` is deduped, capped at `config.QURL_SEND_MAX_RECIPIENTS` (the
// same per-send cap the in-channel form enforces), and excludes the
// invoking user + bots (matches the form's self-send + bot rejection).
//
// Role expansion uses interaction.guild.members.cache. If the cache is
// cold (DM context, bot just restarted) the role expansion silently
// resolves to no members for that role — the role mention lands in
// `invalidTokens` so the caller can surface "couldn't expand @team-blue,
// pick users from the menu instead." This matches the existing form's
// posture (fall through to the picker rather than 404 a slash command).

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
// (10MB of `<@1>` repetitions) doesn't tie up the worker. The form's
// 200-char descriptions and Discord's natural 2000-char message cap
// make 4000 chars more than enough headroom for legitimate use.
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
    return { ids: [], invalidTokens: [] };
  }
  // Length-cap BEFORE regex matching to keep the global-flag iteration
  // bounded under adversarial input. The /g flag scans linearly but
  // pathological repetitions still allocate per-match strings. If the
  // cut lands inside a `<...>` token, drop the trailing partial so the
  // strip-pass doesn't surface a manufactured "invalid token" the user
  // didn't actually type wrong.
  let input = raw.length > MAX_INPUT_LENGTH ? raw.slice(0, MAX_INPUT_LENGTH) : raw;
  if (input.length === MAX_INPUT_LENGTH) {
    const lastOpen = input.lastIndexOf('<');
    const lastClose = input.lastIndexOf('>');
    if (lastOpen > lastClose) {
      input = input.slice(0, lastOpen);
    }
  }
  if (input.length === 0) {
    return { ids: [], invalidTokens: [] };
  }

  const ids = new Set();
  const invalidTokens = [];
  const senderId = interaction.user?.id;
  const guild = interaction.guild;

  for (const m of input.matchAll(USER_MENTION_RE)) {
    const id = m[1];
    if (id === senderId) continue;
    // Bot filter — best-effort via cache. A cache miss leaves the bot
    // in the result; downstream send-pipeline has its own bot check
    // (see existing form's `Cannot send to a bot` warning path) so
    // this layer being lossy is acceptable.
    const member = guild?.members?.cache?.get(id);
    if (member?.user?.bot) continue;
    ids.add(id);
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
    let added = 0;
    for (const [memberId, member] of members) {
      if (memberId === senderId) continue;
      if (member?.user?.bot) continue;
      ids.add(memberId);
      added++;
    }
    if (added === 0) {
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
  const stripped = input
    .replace(USER_MENTION_RE, ' ')
    .replace(ROLE_MENTION_RE, ' ');
  const leftover = stripped.split(/[\s,]+/).filter(Boolean);
  for (const tok of leftover) {
    invalidTokens.push(tok);
  }

  // Apply the per-send recipient cap. The cap is enforced by the
  // back-half too, but we want the user-visible feedback to say
  // "I capped at N" before they hit Send — not as a back-half error.
  // Config validates QURL_SEND_MAX_RECIPIENTS via intEnv minPositive
  // (see src/config.js), so trust the invariant — no paranoid guards.
  const cap = config.QURL_SEND_MAX_RECIPIENTS;
  let finalIds = [...ids];
  if (finalIds.length > cap) {
    logger.debug('recipient-parser: capping recipient list at QURL_SEND_MAX_RECIPIENTS', {
      raw_count: finalIds.length, cap,
    });
    finalIds = finalIds.slice(0, cap);
  }

  return { ids: finalIds, invalidTokens };
}

module.exports = {
  parseRecipientMentions,
  MAX_INPUT_LENGTH,
};
