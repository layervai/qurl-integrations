/**
 * Unit tests for src/recipient-parser.js — the `recipients:` slash-
 * option mention parser used by /qurl file and /qurl map.
 *
 * Mocks config (just QURL_SEND_MAX_RECIPIENTS) and uses synthetic
 * `interaction` objects with the guild.members.cache / guild.roles.cache
 * shape discord.js produces. No real Discord API.
 */

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

// Mock cap chosen for test convenience (prod default is 50 — see
// QURL_SEND_MAX_RECIPIENTS in src/config.js). Tests pin the cap
// behavior at 25 so the boundary cases stay small + readable.
jest.mock('../src/config', () => ({
  QURL_SEND_MAX_RECIPIENTS: 25,
}));

const { parseRecipientMentions, MAX_INPUT_LENGTH, MAX_INVALID_TOKEN_LENGTH } = require('../src/recipient-parser');
const logger = require('../src/logger');

// Build a synthetic interaction with the cache shape the parser reads.
// Pass `users` as { id → { bot } } and `roles` as
// { id → [member_id, member_id, ...] }. Sender defaults to '900000000000000001'.
// IDs are all-numeric to match real Discord snowflakes — the parser's
// regex is `/<@!?(\d+)>/` so letter-prefixed test fixtures would silently
// drop every mention, masking real coverage.
function makeInteraction({ senderId = '900000000000000001', users = {}, roles = {} } = {}) {
  const memberCache = new Map();
  for (const [id, attrs] of Object.entries(users)) {
    memberCache.set(id, { user: { id, bot: !!attrs.bot } });
  }
  // Discord's role.members is a Collection<id, GuildMember>. We
  // simulate with a Map; the parser iterates with for-of which both
  // honor. Add the member to the cache when it's listed under a role
  // so the bot filter has something to look up.
  const roleCache = new Map();
  for (const [roleId, memberIds] of Object.entries(roles)) {
    const roleMembers = new Map();
    for (const mid of memberIds) {
      const existing = memberCache.get(mid) ?? { user: { id: mid, bot: false } };
      memberCache.set(mid, existing);
      roleMembers.set(mid, existing);
    }
    roleCache.set(roleId, { members: roleMembers });
  }
  return {
    user: { id: senderId },
    guild: {
      members: { cache: memberCache },
      roles: { cache: roleCache },
    },
  };
}

describe('parseRecipientMentions — basic shape', () => {
  test('returns empty result for null/undefined/empty input', () => {
    const int = makeInteraction();
    expect(parseRecipientMentions(null, int)).toEqual({ ids: [], invalidTokens: [], cappedCount: 0 });
    expect(parseRecipientMentions(undefined, int)).toEqual({ ids: [], invalidTokens: [], cappedCount: 0 });
    expect(parseRecipientMentions('', int)).toEqual({ ids: [], invalidTokens: [], cappedCount: 0 });
    expect(parseRecipientMentions('   \t\n', int)).toEqual({ ids: [], invalidTokens: [], cappedCount: 0 });
  });

  test('returns empty result when raw is a non-string (defense vs caller bugs)', () => {
    const int = makeInteraction();
    expect(parseRecipientMentions(42, int)).toEqual({ ids: [], invalidTokens: [], cappedCount: 0 });
    expect(parseRecipientMentions({}, int)).toEqual({ ids: [], invalidTokens: [], cappedCount: 0 });
    expect(parseRecipientMentions([], int)).toEqual({ ids: [], invalidTokens: [], cappedCount: 0 });
  });

  test('extracts a single user mention', () => {
    const int = makeInteraction({ users: { '111111111111111111': {} } });
    expect(parseRecipientMentions('<@111111111111111111>', int))
      .toEqual({ ids: ['111111111111111111'], invalidTokens: [], cappedCount: 0 });
  });

  test('accepts both <@id> and <@!id> forms (legacy nickname mention)', () => {
    const int = makeInteraction({
      users: { '111111111111111111': {}, '222222222222222222': {} },
    });
    expect(parseRecipientMentions('<@111111111111111111> <@!222222222222222222>', int))
      .toEqual({ ids: ['111111111111111111', '222222222222222222'], invalidTokens: [], cappedCount: 0 });
  });

  test('dedupes repeated mentions', () => {
    const int = makeInteraction({ users: { '111111111111111111': {} } });
    expect(parseRecipientMentions('<@111111111111111111> <@111111111111111111> <@!111111111111111111>', int))
      .toEqual({ ids: ['111111111111111111'], invalidTokens: [], cappedCount: 0 });
  });

  test('cappedCount is 0 when input has no mentions (no false-positive cap signal)', () => {
    // Empty-input tests above pin null/undefined/'' paths; this pins
    // the "input had content but no mentions" case explicitly so a
    // future caller reading `cappedCount > 0` to infer "user pasted
    // too many" can't accidentally fire on plain bare-name input.
    const int = makeInteraction();
    const res = parseRecipientMentions('alice bob carol', int);
    expect(res.ids).toEqual([]);
    expect(res.cappedCount).toBe(0);
    expect(res.invalidTokens).toEqual(['alice', 'bob', 'carol']);
  });

  test('handles whitespace + comma + mixed separators', () => {
    const int = makeInteraction({
      users: { '111': {}, '222': {}, '333': {} },
    });
    expect(parseRecipientMentions('<@111>,<@222>  <@333>', int).ids)
      .toEqual(['111', '222', '333']);
  });
});

describe('parseRecipientMentions — filtering', () => {
  test('excludes the sender (no self-sends)', () => {
    const int = makeInteraction({
      senderId: '900000000000000001',
      users: { '900000000000000001': {}, '222222222222222222': {} },
    });
    expect(parseRecipientMentions('<@900000000000000001> <@222222222222222222>', int))
      .toEqual({ ids: ['222222222222222222'], invalidTokens: [], cappedCount: 0 });
  });

  test('excludes bots flagged in the member cache', () => {
    const int = makeInteraction({
      users: { '111': { bot: true }, '222': {} },
    });
    expect(parseRecipientMentions('<@111> <@222>', int))
      .toEqual({ ids: ['222'], invalidTokens: [], cappedCount: 0 });
  });

  test('completely empty interaction ({}) does not throw, returns empty result', () => {
    // The `interaction == null` early-return handles null/undefined,
    // but a truthy empty object still has to walk the optional chains
    // (`interaction.user?.id`, `interaction.guild?.roles?.cache`)
    // without crashing. Pin that an `interaction = {}` is tolerated
    // — a future refactor that switched `.guild?.x` to `.guild.x?` on
    // the assumption "we've already null-checked" would surface here.
    expect(parseRecipientMentions('<@111>', {}))
      .toEqual({ ids: ['111'], invalidTokens: [], cappedCount: 0 });
  });

  test('missing interaction.user falls through (sender-exclusion no-ops, no throw)', () => {
    // Defensive precondition: `interaction.user?.id` is the sender
    // exclusion anchor. If a caller passes `interaction = { guild }`
    // (no user — bot misuse, not user input), the optional chain
    // makes `senderId === undefined` so sender exclusion silently
    // no-ops. Pin that the parser doesn't throw — the caller bug
    // surfaces downstream via the back-half's interaction.user.id
    // read, which is a clearer crash site than a parse-time TypeError.
    const interaction = { guild: { members: { cache: new Map([['111', { user: { id: '111', bot: false } }]]) }, roles: { cache: new Map() } } };
    expect(parseRecipientMentions('<@111>', interaction))
      .toEqual({ ids: ['111'], invalidTokens: [], cappedCount: 0 });
  });

  test('best-effort bot filter: cache miss leaves the ID in (back-half re-checks)', () => {
    // Bot filter relies on member.cache; a cache miss (e.g. cold cache
    // or member not yet fetched) cannot tell us "is this a bot." The
    // parser keeps the ID and lets the downstream send-pipeline's bot
    // check (existing form's "Cannot send to a bot" path) catch it.
    const int = makeInteraction({});  // empty cache
    expect(parseRecipientMentions('<@555>', int))
      .toEqual({ ids: ['555'], invalidTokens: [], cappedCount: 0 });
  });
});

describe('parseRecipientMentions — role mentions', () => {
  test('expands a role to its current members', () => {
    const int = makeInteraction({
      users: { '101': {}, '102': {}, '103': {} },
      roles: { '7000': ['101', '102', '103'] },
    });
    expect(parseRecipientMentions('<@&7000>', int))
      .toEqual({ ids: ['101', '102', '103'], invalidTokens: [], cappedCount: 0 });
  });

  test('merges role expansion with direct user mentions, deduped', () => {
    const int = makeInteraction({
      users: { '101': {}, '102': {}, '103': {} },
      roles: { '7000': ['101', '102'] },
    });
    expect(parseRecipientMentions('<@103> <@&7000> <@101>', int).ids.sort())
      .toEqual(['101', '102', '103']);
  });

  test('role expansion excludes sender and bots', () => {
    const int = makeInteraction({
      senderId: '900',
      users: { '900': {}, '801': { bot: true }, '101': {} },
      roles: { '7000': ['900', '801', '101'] },
    });
    expect(parseRecipientMentions('<@&7000>', int))
      .toEqual({ ids: ['101'], invalidTokens: [], cappedCount: 0 });
  });

  test('role unknown to the guild lands in invalidTokens', () => {
    const int = makeInteraction({});
    expect(parseRecipientMentions('<@&7000>', int))
      .toEqual({ ids: [], invalidTokens: ['<@&7000>'], cappedCount: 0 });
  });

  test('role with no usable members (all sender/bots) lands in invalidTokens', () => {
    const int = makeInteraction({
      senderId: '900',
      users: { '900': {}, '801': { bot: true } },
      roles: { '7000': ['900', '801'] },
    });
    const res = parseRecipientMentions('<@&7000>', int);
    expect(res.ids).toEqual([]);
    expect(res.invalidTokens).toEqual(['<@&7000>']);
  });

  test('DM context (guild=undefined) treats role mentions as invalid', () => {
    const int = { user: { id: '900' }, guild: undefined };
    expect(parseRecipientMentions('<@&7000>', int))
      .toEqual({ ids: [], invalidTokens: ['<@&7000>'], cappedCount: 0 });
  });

  test('DM context (guild=null) also treats role mentions as invalid', () => {
    // discord.js returns `null` (not `undefined`) for DM-context
    // guilds. Optional chaining handles both, but pin the runtime
    // shape explicitly so a future refactor that switched
    // `guild?.roles?.cache` to `guild.roles?.cache` would surface
    // on this test, not in prod.
    const int = { user: { id: '900' }, guild: null };
    expect(parseRecipientMentions('<@&7000>', int))
      .toEqual({ ids: [], invalidTokens: ['<@&7000>'], cappedCount: 0 });
  });

  test('repeated residue tokens dedupe in invalidTokens (symmetric with role-error dedup)', () => {
    // `<#456> <#456>` previously surfaced two entries; now dedupes
    // via `residueSeen` (parallel to the role-error path's
    // `invalidRoleIds`). Caller's "couldn't parse: X, X" embed
    // isn't user-hostile.
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('<@111> <#456> <#456>', int))
      .toEqual({ ids: ['111'], invalidTokens: ['<#456>'], cappedCount: 0 });
    expect(parseRecipientMentions('<@111> alice alice bob alice', int))
      .toEqual({ ids: ['111'], invalidTokens: ['alice', 'bob'], cappedCount: 0 });
  });

  test('repeated invalid role mention dedupes in invalidTokens', () => {
    // `<@&999> <@&999>` against a guild missing role 999 must yield
    // ONE invalidTokens entry, not two. Without the role-id dedupe,
    // matchAll iterates each input occurrence and pushes the raw
    // token each time. Symmetric with the residue dedupe above.
    const int = makeInteraction({});  // no role 999
    expect(parseRecipientMentions('<@&999> <@&999> <@&999>', int))
      .toEqual({ ids: [], invalidTokens: ['<@&999>'], cappedCount: 0 });
  });

  test('direct mentions always win the cap over role-expansion members', () => {
    // Pass 1 (user mentions) runs before pass 2 (role expansion), so
    // direct mentions are inserted into `ids` first and survive the
    // cap. A future refactor that swapped pass order — or interleaved
    // mention/role parsing — could silently displace direct mentions
    // by large role expansions. Pin the invariant.
    const roleMembers = [];
    const users = {};
    // 5 direct-mention users (distinct snowflake range so we can
    // assert presence without ambiguity).
    const directIds = ['8000000001', '8000000002', '8000000003', '8000000004', '8000000005'];
    for (const id of directIds) users[id] = {};
    for (let i = 0; i < 50; i++) {
      const id = `${5000000000 + i}`;
      users[id] = {};
      roleMembers.push(id);
    }
    const int = makeInteraction({ users, roles: { '7000': roleMembers } });
    // Role mention FIRST in input order; direct mentions follow.
    // With cap=25, the 5 direct mentions must all survive — the
    // role's overflow goes to cappedCount, not the direct slots.
    const directMentions = directIds.map(id => `<@${id}>`).join(' ');
    const res = parseRecipientMentions(`<@&7000> ${directMentions}`, int);
    expect(res.ids).toHaveLength(25);
    expect(res.ids).toEqual(expect.arrayContaining(directIds));
    // 55 unique candidates - 25 cap = 30 dropped.
    expect(res.cappedCount).toBe(30);
  });

  test('user listed BOTH directly AND via a role mention appears once in ids, role is not flagged useless', () => {
    // The "merges role expansion with direct user mentions" test
    // above exercises this implicitly (user 101 is in role 7000 AND
    // mentioned directly), but a fixture that EXPLICITLY overlaps
    // makes two invariants non-incidental:
    // (a) ids.length === 1 (Set dedup — a regression re-adding role
    //     members without the dedupe guard surfaces here);
    // (b) invalidTokens === [] (the role contributed via dedupe so
    //     `usable++` fires before the seen-check — a refactor
    //     swapping `usable++` to AFTER `seen.has` would flag this
    //     role as useless and push the token to invalidTokens).
    const int = makeInteraction({
      users: { '101': {} },
      roles: { '7000': ['101'] },
    });
    expect(parseRecipientMentions('<@101> <@&7000>', int))
      .toEqual({ ids: ['101'], invalidTokens: [], cappedCount: 0 });
  });

  test('role mention repeated in input expands once (no double-counting)', () => {
    // matchAll iterates every regex hit, so a repeated `<@&id>` would
    // re-enter the expansion loop and re-add the same members. ids is
    // a Set so they dedupe; pin that AND that invalidTokens stays empty
    // (the second hit shouldn't trip the "no usable members" path on a
    // role whose members were already added).
    const int = makeInteraction({
      users: { '101': {}, '102': {} },
      roles: { '7000': ['101', '102'] },
    });
    const res = parseRecipientMentions('<@&7000> <@&7000> <@&7000>', int);
    expect(res.ids.sort()).toEqual(['101', '102']);
    expect(res.invalidTokens).toEqual([]);
  });
});

describe('parseRecipientMentions — invalid tokens', () => {
  test('channel mentions land in invalidTokens', () => {
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('<@111> <#456>', int))
      .toEqual({ ids: ['111'], invalidTokens: ['<#456>'], cappedCount: 0 });
  });

  test('custom emoji land in invalidTokens', () => {
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('<@111> <:smile:789>', int))
      .toEqual({ ids: ['111'], invalidTokens: ['<:smile:789>'], cappedCount: 0 });
  });

  test('bare plaintext usernames land in invalidTokens', () => {
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('alice <@111> bob', int))
      .toEqual({ ids: ['111'], invalidTokens: ['alice', 'bob'], cappedCount: 0 });
  });

  test('mention with missing closer (truncated input) is treated as invalid', () => {
    const int = makeInteraction({ users: { '111': {} } });
    const res = parseRecipientMentions('<@111 incomplete', int);
    // The truncated `<@111` cannot match the user-mention regex
    // (requires `>`), so it falls into the residual-token bucket.
    expect(res.ids).toEqual([]);
    expect(res.invalidTokens.length).toBeGreaterThan(0);
  });

  test('@everyone / @here are pre-escaped with zero-width-space in invalidTokens', () => {
    // Discord's @everyone / @here are NOT `<@id>` mentions — they're
    // bare tokens that the API expands at message-send time. A caller
    // naively interpolating `invalidTokens` into a user-visible
    // message (`` `Couldn't parse: ${invalidTokens.join(', ')}` ``)
    // would fan-out-ping the channel. To make the boundary safe by
    // default, the parser inserts a zero-width space (U+200B) after
    // the `@` — visually identical, but Discord's tokenizer no longer
    // recognizes the mass-mention shape.
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('@everyone <@111>', int))
      .toEqual({ ids: ['111'], invalidTokens: ['@\u200beveryone'], cappedCount: 0 });
    expect(parseRecipientMentions('@here <@111>', int))
      .toEqual({ ids: ['111'], invalidTokens: ['@\u200bhere'], cappedCount: 0 });
  });

  test('@Everyone / @Here (capitalized) are NOT escaped — Discord parser is lowercase-only', () => {
    // Discord's mass-mention tokenizer is itself case-sensitive
    // (lowercase only). The parser's escape regex is `/@(everyone|here)/g`
    // — INTENTIONALLY no `/i` flag. Widening to case-insensitive
    // would needlessly mangle legitimate `@Everyone` paste artifacts.
    // Pin the invariant so a future "defensive hardening" PR can't
    // silently widen the regex without flipping this test.
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('@Everyone <@111>', int))
      .toEqual({ ids: ['111'], invalidTokens: ['@Everyone'], cappedCount: 0 });
    expect(parseRecipientMentions('@Here <@111>', int))
      .toEqual({ ids: ['111'], invalidTokens: ['@Here'], cappedCount: 0 });
  });

  test('@everyone with trailing punctuation is also escaped (single-token residue)', () => {
    // The strip-pass split class `[\s,;|/]+` does NOT include `.`,
    // `:`, `!`, `?`, `-`, so `@everyone!` and `@everyone.fix` survive
    // as single tokens. Exact-match escape would slip these past the
    // guard. Pin that the regex-based escape catches them.
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('@everyone! <@111>', int))
      .toEqual({ ids: ['111'], invalidTokens: ['@\u200beveryone!'], cappedCount: 0 });
    expect(parseRecipientMentions('@everyone.fix <@111>', int))
      .toEqual({ ids: ['111'], invalidTokens: ['@\u200beveryone.fix'], cappedCount: 0 });
    expect(parseRecipientMentions('@here: <@111>', int))
      .toEqual({ ids: ['111'], invalidTokens: ['@\u200bhere:'], cappedCount: 0 });
    // Embedded mid-token (paste artifact). Regex replaces ALL
    // occurrences so no `@everyone` / `@here` substring escapes.
    expect(parseRecipientMentions('here@everyone <@111>', int))
      .toEqual({ ids: ['111'], invalidTokens: ['here@\u200beveryone'], cappedCount: 0 });
  });

  test('newline characters separate tokens (split regex includes \\n)', () => {
    // Discord slash-command option strings can contain literal
    // newlines (paste-from-multi-line). Pin that the split regex
    // `/[\s,;|/]+/` includes \n — a regression that switched to
    // `/[ \t,;|/]+/` would silently merge "alice\nbob" into one
    // bare-name token.
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('<@111>\n<#456>\n\nstray', int))
      .toEqual({ ids: ['111'], invalidTokens: ['<#456>', 'stray'], cappedCount: 0 });
  });
});

describe('parseRecipientMentions — cap + length safety', () => {
  test('caps the result at QURL_SEND_MAX_RECIPIENTS + reports cappedCount', () => {
    // Build 30 user mentions; cap is 25 (set in the jest.mock above).
    // IDs are numeric to match the real Discord-mention format the
    // parser regex requires.
    const users = {};
    const mentions = [];
    for (let i = 0; i < 30; i++) {
      const id = `${1000000000 + i}`;
      users[id] = {};
      mentions.push(`<@${id}>`);
    }
    const int = makeInteraction({ users });
    const res = parseRecipientMentions(mentions.join(' '), int);
    expect(res.ids).toHaveLength(25);
    // First-N ordering (set insertion is preserved). Pin that the
    // cap takes the first 25 — keeping order stable means a user
    // can reorder their mentions to influence the kept set.
    expect(res.ids[0]).toBe('1000000000');
    expect(res.ids[24]).toBe('1000000024');
    // cappedCount lets the caller surface "I kept 25 of 30" without
    // re-parsing or re-counting input.
    expect(res.cappedCount).toBe(5);
  });

  test('truncates input above MAX_INPUT_LENGTH before scanning (ReDoS guard)', () => {
    // Build a string longer than MAX_INPUT_LENGTH with a real mention
    // at the END — the parser should NOT find it after truncation. If
    // a future regression dropped the length cap, this mention would
    // surface and the test would flip.
    const padding = 'x'.repeat(MAX_INPUT_LENGTH + 100);
    const int = makeInteraction({ users: { '999': {} } });
    const res = parseRecipientMentions(padding + ' <@999>', int);
    expect(res.ids).toEqual([]);
  });

  test('input of exactly MAX_INPUT_LENGTH is NOT truncated (boundary check)', () => {
    // Pin the inequality direction. The cap fires on `raw.length > MAX_INPUT_LENGTH`,
    // not >=, so an exactly-MAX-length input should be processed in
    // full. Build a string of exactly MAX_INPUT_LENGTH that contains
    // a real mention near the start so the rest is harmless padding.
    const mention = '<@777>';
    const padding = 'x'.repeat(MAX_INPUT_LENGTH - mention.length);
    const input = mention + padding;
    expect(input).toHaveLength(MAX_INPUT_LENGTH);
    const int = makeInteraction({ users: { '777': {} } });
    expect(parseRecipientMentions(input, int).ids).toEqual(['777']);
  });

  test('cappedCount reflects POST-dedupe count, not raw mention count', () => {
    // Cap fires AFTER dedupe (`finalIds = [...ids]`). Pin the order
    // so a future refactor that swapped cap-before-dedupe would
    // surface: paste 60 mentions with heavy duplicates that
    // dedupe to 28 unique → cappedCount should be 3 (= 28 - 25),
    // NOT 35 (= 60 - 25).
    const users = {};
    const mentions = [];
    for (let i = 0; i < 28; i++) {
      const id = `${2000000000 + i}`;
      users[id] = {};
      mentions.push(`<@${id}>`);
    }
    // Duplicate every mention to push raw count to 56, which still
    // dedupes to 28 unique. Then add 4 more dupes of the first to
    // push the raw count even higher without growing the unique set.
    const dupedInput = mentions.concat(mentions).concat([
      `<@${2000000000}>`, `<@${2000000000}>`, `<@${2000000000}>`, `<@${2000000000}>`,
    ]).join(' ');
    const int = makeInteraction({ users });
    const res = parseRecipientMentions(dupedInput, int);
    expect(res.ids).toHaveLength(25);
    expect(res.cappedCount).toBe(3);  // 28 unique - 25 cap
  });

  test('cappedCount accounts for role-expansion overflow (not just direct mentions)', () => {
    // Round-5 cr regression test: a `<@&role1> <@&role2>` where role1
    // fills the cap and role2 has more usable members must NOT
    // silently truncate. Both roles' members count toward
    // `cappedCount`, even though the inner loop short-circuits Set
    // insertions past cap. Without this, callers can't tell users
    // "we kept 25 of N" when the overflow source is role expansion.
    const role1Members = [];
    const role2Members = [];
    const users = {};
    for (let i = 0; i < 50; i++) {
      const id = `${3000000000 + i}`;
      users[id] = {};
      role1Members.push(id);
    }
    for (let i = 0; i < 20; i++) {
      const id = `${4000000000 + i}`;
      users[id] = {};
      role2Members.push(id);
    }
    const int = makeInteraction({
      users,
      roles: { '7000': role1Members, '7001': role2Members },
    });
    const res = parseRecipientMentions('<@&7000> <@&7001>', int);
    expect(res.ids).toHaveLength(25);
    // 50 (role1) + 20 (role2) = 70 unique candidates; 25 kept; 45 dropped.
    expect(res.cappedCount).toBe(45);
    expect(res.invalidTokens).toEqual([]);
  });

  test('cap and invalidTokens are populated independently in the same call', () => {
    // The cap test above exercises 30 valid → 25 ids. The invalid-
    // tokens tests exercise typos in isolation. Pin the cross-cut:
    // 30 valid mentions PLUS a channel-mention typo should produce
    // 25 ids AND keep the typo in invalidTokens. A future bug where
    // the cap pass also accidentally truncated invalidTokens would
    // silently hide user-visible error feedback.
    const users = {};
    const mentions = [];
    for (let i = 0; i < 30; i++) {
      const id = `${1000000000 + i}`;
      users[id] = {};
      mentions.push(`<@${id}>`);
    }
    const int = makeInteraction({ users });
    const res = parseRecipientMentions(`${mentions.join(' ')} <#456>`, int);
    expect(res.ids).toHaveLength(25);
    expect(res.invalidTokens).toEqual(['<#456>']);
  });

  test('truncation that lands inside `<...>` drops the partial (no manufactured invalid token)', () => {
    // If the slice cuts inside an open mention like `<@123`, a naive
    // strip+split would surface `<@123` as an invalid token the user
    // didn't actually mistype. Pin that we trim back to the last `<`
    // before scanning, so the residual-token bucket stays clean.
    const padding = 'x'.repeat(MAX_INPUT_LENGTH - 5);  // leaves room for partial mention at the end
    const input = padding + '<@123456789';  // open `<@…` with no closing `>`, will land past MAX
    expect(input.length).toBeGreaterThan(MAX_INPUT_LENGTH);
    const int = makeInteraction({ users: { '123456789': {} } });
    const res = parseRecipientMentions(input, int);
    expect(res.ids).toEqual([]);
    // padding is one big run of `x`s with no separators, so it
    // surfaces as one invalid token after the strip. The KEY
    // assertion is the partial-mention residue isn't ALSO there.
    expect(res.invalidTokens.every(t => !t.startsWith('<@'))).toBe(true);
  });

  test('mass-mention escape runs BEFORE per-token truncation', () => {
    // Critical ordering. If truncate ran first and the slice fell
    // before the escape pass, a token like `@everyone` near the
    // start of an oversized residue would land in invalidTokens
    // un-escaped after truncation. Pin: with `@everyone` at the
    // start of a 300-char token, the rendered output starts with
    // the ZWS-escaped form (`@\u200beveryone`) AND ends in the
    // truncation marker. A future refactor flipping to truncate-
    // then-escape would lose the ZWS and fail this test.
    const int = makeInteraction({});
    const overCap = `@everyone${'x'.repeat(300)}`;  // 309 chars total
    const res = parseRecipientMentions(overCap, int);
    expect(res.invalidTokens).toHaveLength(1);
    const rendered = res.invalidTokens[0];
    expect(rendered.endsWith('…')).toBe(true);
    expect(rendered.startsWith('@\u200beveryone')).toBe(true);  // ZWS form
    expect(rendered.startsWith('@e')).toBe(false);  // raw form absent
  });

  test('invalidTokens entries are capped at MAX_INVALID_TOKEN_LENGTH', () => {
    // A single ~4000-char garbage token (one long string with no
    // separators) would otherwise blow Discord's 4096-char embed
    // budget if the caller interpolated it verbatim. Parser caps
    // each token at MAX_INVALID_TOKEN_LENGTH chars and appends `…`.
    // Pin both branches: under-cap stays untrimmed; over-cap gets
    // the marker.
    const int = makeInteraction({});
    const longToken = 'x'.repeat(500);
    const res = parseRecipientMentions(longToken, int);
    expect(res.invalidTokens).toHaveLength(1);
    // 256 chars + the ellipsis marker.
    // length = cap + 1 (the ellipsis).
    expect(res.invalidTokens[0]).toHaveLength(MAX_INVALID_TOKEN_LENGTH + 1);
    expect(res.invalidTokens[0].endsWith('…')).toBe(true);

    // Under-cap token is unchanged.
    const shortToken = 'y'.repeat(100);
    const res2 = parseRecipientMentions(shortToken, int);
    expect(res2.invalidTokens).toEqual([shortToken]);
  });

  test('exactly-cap unique candidates: no cap fires, no log call', () => {
    // Boundary on the OTHER side of the cap. The cap fires when
    // `cappedCount > 0` (i.e. seen.size > ids.size); at exactly
    // `cap` unique inputs, ids absorbs all of them and cappedCount
    // stays 0. Pin that this branch doesn't log — the cap-overshoot
    // signal should ONLY surface when something was actually dropped.
    const users = {};
    const mentions = [];
    for (let i = 0; i < 25; i++) {
      const id = `${7000000000 + i}`;
      users[id] = {};
      mentions.push(`<@${id}>`);
    }
    const int = makeInteraction({ users });
    logger.debug.mockClear();
    logger.warn.mockClear();
    const res = parseRecipientMentions(mentions.join(' '), int);
    expect(res.ids).toHaveLength(25);
    expect(res.cappedCount).toBe(0);
    expect(logger.debug).not.toHaveBeenCalled();
    expect(logger.warn).not.toHaveBeenCalled();
  });

  test('cap-overshoot logging escalates to warn past 2x the cap', () => {
    // Modest overshoot (≤ 2× cap) signals "user typed too many" and
    // stays at debug; massive overshoot signals "user pasted an
    // untrimmed list" and surfaces at warn so oncall sees the pattern.
    // Pin the threshold so a future refactor that flipped the
    // comparison or moved the multiplier silently regresses the
    // oncall signal. Also pins the meta payload shape so a future
    // field rename in the log fires this test, not the prod logs.
    const users = {};
    const mentions = [];
    // 26 unique = cap+1 = modest overshoot → debug
    for (let i = 0; i < 26; i++) {
      const id = `${6000000000 + i}`;
      users[id] = {};
      mentions.push(`<@${id}>`);
    }
    let int = makeInteraction({ users });
    logger.debug.mockClear();
    logger.warn.mockClear();
    parseRecipientMentions(mentions.join(' '), int);
    expect(logger.debug).toHaveBeenCalledTimes(1);
    expect(logger.debug).toHaveBeenCalledWith(
      expect.stringContaining('capping recipient list'),
      expect.objectContaining({ unique_count: 26, cap: 25, capped_count: 1 }),
    );
    expect(logger.warn).not.toHaveBeenCalled();

    // 50 unique = exactly 2× cap (the boundary) → debug.
    // Pin the strict-greater direction: `seen.size > cap *
    // MASSIVE_OVERSHOOT_MULTIPLIER` means EXACTLY 2× stays at debug.
    // A future refactor flipping `>` to `>=` would surface here.
    for (let i = 26; i < 50; i++) {
      const id = `${6000000000 + i}`;
      users[id] = {};
      mentions.push(`<@${id}>`);
    }
    int = makeInteraction({ users });
    logger.debug.mockClear();
    logger.warn.mockClear();
    parseRecipientMentions(mentions.join(' '), int);
    expect(logger.debug).toHaveBeenCalledTimes(1);
    expect(logger.warn).not.toHaveBeenCalled();

    // 51 unique = 2× cap + 1 = massive overshoot → warn
    for (let i = 26; i < 51; i++) {
      const id = `${6000000000 + i}`;
      users[id] = {};
      mentions.push(`<@${id}>`);
    }
    int = makeInteraction({ users });
    logger.debug.mockClear();
    logger.warn.mockClear();
    parseRecipientMentions(mentions.join(' '), int);
    expect(logger.warn).toHaveBeenCalledTimes(1);
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringContaining('capping recipient list'),
      expect.objectContaining({ unique_count: 51, cap: 25, capped_count: 26 }),
    );
    expect(logger.debug).not.toHaveBeenCalled();
  });
});
