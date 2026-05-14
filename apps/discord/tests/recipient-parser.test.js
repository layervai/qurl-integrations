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
    expect(parseRecipientMentions(null, int)).toMatchObject({ ids: [], invalidTokens: [], cappedCount: 0 });
    expect(parseRecipientMentions(undefined, int)).toMatchObject({ ids: [], invalidTokens: [], cappedCount: 0 });
    expect(parseRecipientMentions('', int)).toMatchObject({ ids: [], invalidTokens: [], cappedCount: 0 });
    expect(parseRecipientMentions('   \t\n', int)).toMatchObject({ ids: [], invalidTokens: [], cappedCount: 0 });
  });

  test('returns empty result when raw is a non-string (defense vs caller bugs)', () => {
    const int = makeInteraction();
    expect(parseRecipientMentions(42, int)).toMatchObject({ ids: [], invalidTokens: [], cappedCount: 0 });
    expect(parseRecipientMentions({}, int)).toMatchObject({ ids: [], invalidTokens: [], cappedCount: 0 });
    expect(parseRecipientMentions([], int)).toMatchObject({ ids: [], invalidTokens: [], cappedCount: 0 });
  });

  test('extracts a single user mention', () => {
    const int = makeInteraction({ users: { '111111111111111111': {} } });
    expect(parseRecipientMentions('<@111111111111111111>', int))
      .toMatchObject({ ids: ['111111111111111111'], invalidTokens: [], cappedCount: 0 });
  });

  test('accepts both <@id> and <@!id> forms (legacy nickname mention)', () => {
    const int = makeInteraction({
      users: { '111111111111111111': {}, '222222222222222222': {} },
    });
    expect(parseRecipientMentions('<@111111111111111111> <@!222222222222222222>', int))
      .toMatchObject({ ids: ['111111111111111111', '222222222222222222'], invalidTokens: [], cappedCount: 0 });
  });

  test('dedupes repeated mentions', () => {
    const int = makeInteraction({ users: { '111111111111111111': {} } });
    expect(parseRecipientMentions('<@111111111111111111> <@111111111111111111> <@!111111111111111111>', int))
      .toMatchObject({ ids: ['111111111111111111'], invalidTokens: [], cappedCount: 0 });
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
  test('keeps the sender (self-send is supported)', () => {
    // Self-send: the sender deliberately included themselves via
    // `@me`. Parser does NOT filter — the confirm card surfaces a
    // neutral "Send includes you." notice instead.
    const int = makeInteraction({
      senderId: '900000000000000001',
      users: { '900000000000000001': {}, '222222222222222222': {} },
    });
    expect(parseRecipientMentions('<@900000000000000001> <@222222222222222222>', int).ids.sort())
      .toEqual(['222222222222222222', '900000000000000001']);
  });

  test('sender alone is a legitimate single-recipient self-send', () => {
    const int = makeInteraction({
      senderId: '900000000000000001',
      users: { '900000000000000001': {} },
    });
    expect(parseRecipientMentions('<@900000000000000001>', int))
      .toMatchObject({ ids: ['900000000000000001'], invalidTokens: [], cappedCount: 0 });
  });

  test('excludes bots flagged in the member cache', () => {
    const int = makeInteraction({
      users: { '111': { bot: true }, '222': {} },
    });
    expect(parseRecipientMentions('<@111> <@222>', int))
      .toMatchObject({ ids: ['222'], invalidTokens: [], cappedCount: 0 });
  });

  test('completely empty interaction ({}) does not throw, returns empty result', () => {
    // The `interaction == null` early-return handles null/undefined,
    // but a truthy empty object still has to walk the optional chains
    // (`interaction.guild?.roles?.cache`) without crashing. Pin that
    // `interaction = {}` is tolerated — a future refactor that
    // switched `.guild?.x` to `.guild.x?` on the assumption "we've
    // already null-checked" would surface here.
    expect(parseRecipientMentions('<@111>', {}))
      .toMatchObject({ ids: ['111'], invalidTokens: [], cappedCount: 0 });
  });

  test('best-effort bot filter: cache miss leaves the ID in (back-half re-checks)', () => {
    // Bot filter relies on member.cache; a cache miss (e.g. cold cache
    // or member not yet fetched) cannot tell us "is this a bot." The
    // parser keeps the ID and lets the downstream send-pipeline's bot
    // check (existing form's "Cannot send to a bot" path) catch it.
    const int = makeInteraction({});  // empty cache
    expect(parseRecipientMentions('<@555>', int))
      .toMatchObject({ ids: ['555'], invalidTokens: [], cappedCount: 0 });
  });
});

describe('parseRecipientMentions — role mentions', () => {
  test('expands a role to its current members', () => {
    const int = makeInteraction({
      users: { '101': {}, '102': {}, '103': {} },
      roles: { '7000': ['101', '102', '103'] },
    });
    expect(parseRecipientMentions('<@&7000>', int))
      .toMatchObject({ ids: ['101', '102', '103'], invalidTokens: [], cappedCount: 0 });
  });

  test('merges role expansion with direct user mentions, deduped', () => {
    const int = makeInteraction({
      users: { '101': {}, '102': {}, '103': {} },
      roles: { '7000': ['101', '102'] },
    });
    expect(parseRecipientMentions('<@103> <@&7000> <@101>', int).ids.sort())
      .toEqual(['101', '102', '103']);
  });

  test('role expansion includes sender but excludes bots', () => {
    // Self-send: sender expanded out of a role mention stays in the
    // recipient set. Bots in the role are still filtered.
    const int = makeInteraction({
      senderId: '900',
      users: { '900': {}, '801': { bot: true }, '101': {} },
      roles: { '7000': ['900', '801', '101'] },
    });
    expect(parseRecipientMentions('<@&7000>', int).ids.sort())
      .toEqual(['101', '900']);
  });

  test('role unknown to the guild lands in invalidTokens', () => {
    const int = makeInteraction({});
    expect(parseRecipientMentions('<@&7000>', int))
      .toMatchObject({ ids: [], invalidTokens: ['<@&7000>'], cappedCount: 0 });
  });

  test('role with no usable members (all bots) lands in invalidTokens', () => {
    const int = makeInteraction({
      senderId: '900',
      users: { '801': { bot: true } },
      roles: { '7000': ['801'] },
    });
    const res = parseRecipientMentions('<@&7000>', int);
    expect(res.ids).toEqual([]);
    expect(res.invalidTokens).toEqual(['<@&7000>']);
  });

  test('role with only the sender expands to the sender (self-only role is valid)', () => {
    const int = makeInteraction({
      senderId: '900',
      users: { '900': {} },
      roles: { '7000': ['900'] },
    });
    expect(parseRecipientMentions('<@&7000>', int))
      .toMatchObject({ ids: ['900'], invalidTokens: [], cappedCount: 0 });
  });

  test('DM context (guild=undefined) treats role mentions as invalid', () => {
    const int = { user: { id: '900' }, guild: undefined };
    expect(parseRecipientMentions('<@&7000>', int))
      .toMatchObject({ ids: [], invalidTokens: ['<@&7000>'], cappedCount: 0 });
  });

  test('DM context (guild=null) also treats role mentions as invalid', () => {
    // discord.js returns `null` (not `undefined`) for DM-context
    // guilds. Optional chaining handles both, but pin the runtime
    // shape explicitly so a future refactor that switched
    // `guild?.roles?.cache` to `guild.roles?.cache` would surface
    // on this test, not in prod.
    const int = { user: { id: '900' }, guild: null };
    expect(parseRecipientMentions('<@&7000>', int))
      .toMatchObject({ ids: [], invalidTokens: ['<@&7000>'], cappedCount: 0 });
  });

  test('repeated residue tokens dedupe in invalidTokens (symmetric with role-error dedup)', () => {
    // `<#456> <#456>` previously surfaced two entries; now dedupes
    // via `residueSeen` (parallel to the role-error path's
    // `invalidRoleIds`). Caller's "couldn't parse: X, X" embed
    // isn't user-hostile.
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('<@111> <#456> <#456>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['<#456>'], cappedCount: 0 });
    expect(parseRecipientMentions('<@111> alice alice bob alice', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['alice', 'bob'], cappedCount: 0 });
  });

  test('repeated invalid role mention dedupes in invalidTokens', () => {
    // `<@&999> <@&999>` against a guild missing role 999 must yield
    // ONE invalidTokens entry, not two. Without the role-id dedupe,
    // matchAll iterates each input occurrence and pushes the raw
    // token each time. Symmetric with the residue dedupe above.
    const int = makeInteraction({});  // no role 999
    expect(parseRecipientMentions('<@&999> <@&999> <@&999>', int))
      .toMatchObject({ ids: [], invalidTokens: ['<@&999>'], cappedCount: 0 });
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
      .toMatchObject({ ids: ['101'], invalidTokens: [], cappedCount: 0 });
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

describe('parseRecipientMentions — @everyone (allowMassMention)', () => {
  test('allowed: expands to all non-bot guild members', () => {
    const int = makeInteraction({
      users: { '101': {}, '102': {}, '801': { bot: true }, '103': {} },
    });
    const res = parseRecipientMentions('@everyone', int, { allowMassMention: true });
    expect(res.ids.sort()).toEqual(['101', '102', '103']);
    expect(res.invalidTokens).toEqual([]);
    expect(res.massMentionDenied).toBe(false);
  });

  test('allowed: merges with direct mentions, deduped', () => {
    // Same user reached via both `@everyone` and a direct `<@id>` is
    // contributed once (the parser uses a single `consider()` that
    // dedupes via `seen`). The role-expansion test pins the same
    // contract for roles; this pins it for the mass-mention path.
    const int = makeInteraction({
      users: { '101': {}, '102': {} },
    });
    const res = parseRecipientMentions('<@101> @everyone', int, { allowMassMention: true });
    expect(res.ids.sort()).toEqual(['101', '102']);
  });

  test('denied (default): surfaces massMentionDenied=true, no expansion, no invalidTokens entry', () => {
    const int = makeInteraction({
      users: { '101': {}, '102': {} },
    });
    const res = parseRecipientMentions('@everyone', int);
    expect(res.ids).toEqual([]);
    expect(res.invalidTokens).toEqual([]);
    expect(res.massMentionDenied).toBe(true);
  });

  test('denied: explicit allowMassMention=false is equivalent to default', () => {
    const int = makeInteraction({ users: { '101': {} } });
    expect(parseRecipientMentions('@everyone <@101>', int, { allowMassMention: false }))
      .toMatchObject({ ids: ['101'], invalidTokens: [], massMentionDenied: true });
  });

  test('no @everyone in input → massMentionDenied=false regardless of permission', () => {
    const int = makeInteraction({ users: { '101': {} } });
    // Allowed but not used.
    expect(parseRecipientMentions('<@101>', int, { allowMassMention: true })
      .massMentionDenied).toBe(false);
    // Denied but not attempted.
    expect(parseRecipientMentions('<@101>', int, { allowMassMention: false })
      .massMentionDenied).toBe(false);
  });

  test('allowed: bot filter applies during expansion', () => {
    const int = makeInteraction({
      users: { '101': {}, '801': { bot: true }, '802': { bot: true } },
    });
    const res = parseRecipientMentions('@everyone', int, { allowMassMention: true });
    expect(res.ids).toEqual(['101']);
  });

  test('allowed with no guild member cache returns empty (DM-context guard)', () => {
    // DM-context guild=null/undefined: expansion is impossible, but
    // the parser should NOT throw. Returns empty ids, massMentionDenied
    // stays false (the user HAD permission, the cache just had nothing).
    const int = { user: { id: '900' }, guild: undefined };
    const res = parseRecipientMentions('@everyone', int, { allowMassMention: true });
    expect(res.ids).toEqual([]);
    expect(res.massMentionDenied).toBe(false);
  });

  test('allowed but every cached member is a bot → empty expansion, massMentionDenied still false', () => {
    // Distinguish "expanded to nothing" (cache has only bots) from
    // "denied" (no MENTION_EVERYONE perm). The caller renders these
    // differently: "no valid recipients" generic copy vs. the
    // permission-specific @everyone copy. Pin that the allowed-but-
    // empty path stays on the generic side.
    const int = makeInteraction({
      users: { '801': { bot: true }, '802': { bot: true } },
    });
    const res = parseRecipientMentions('@everyone', int, { allowMassMention: true });
    expect(res.ids).toEqual([]);
    expect(res.massMentionDenied).toBe(false);
  });

  test('allowed: cap short-circuits the cache scan (large guild cap behavior)', () => {
    // The @everyone expansion iterates guild.members.cache — a large
    // guild could have 10k+ entries. Once `ids.size === cap`, the
    // loop breaks rather than scanning the remainder. We can't easily
    // assert "break ran" directly without instrumentation, but we
    // can assert the cap-bounded result: 25 (the cap) ids out of 40
    // synthetic non-bot members, with NO cappedCount surfaced for the
    // skipped members (the early break is the tradeoff — we don't
    // count past-cap members in @everyone expansion, unlike the
    // text-mention path).
    const users = {};
    for (let i = 0; i < 40; i++) users[`u${String(i).padStart(18, '0')}`] = {};
    const int = makeInteraction({ users });
    const res = parseRecipientMentions('@everyone', int, { allowMassMention: true });
    expect(res.ids.length).toBe(25);
    // cappedCount reflects only the members the loop actually saw
    // past `ids.size === cap`. Since we break immediately, no members
    // get added to `seen` beyond the cap, so cappedCount stays at 0.
    expect(res.cappedCount).toBe(0);
  });

  test('mixed `@everyone @here` in one input: @everyone gated separately, @here defuses', () => {
    // Both shapes hit different paths simultaneously. With
    // allowMassMention=false: @everyone surfaces massMentionDenied,
    // @here surfaces in invalidTokens via the legacy defuse. Pin
    // the two paths don't interfere.
    const int = makeInteraction({ users: { '111': {} } });
    const res = parseRecipientMentions('@everyone @here <@111>', int);
    expect(res.ids).toEqual(['111']);
    expect(res.massMentionDenied).toBe(true);
    expect(res.invalidTokens).toEqual(['@​here']);
  });

  test('repeated `@everyone @everyone` triggers expansion once (single-shot dedupe via `seen`)', () => {
    // Even though the parser sees `@everyone` twice, the expansion is
    // a single pass — consider() dedupes via `seen`. Pin that a
    // repeated mass-mention doesn't produce duplicate ids.
    const int = makeInteraction({
      users: { '101': {}, '102': {} },
    });
    const res = parseRecipientMentions('@everyone @everyone', int, { allowMassMention: true });
    expect(res.ids.sort()).toEqual(['101', '102']);
    expect(res.invalidTokens).toEqual([]);
    expect(res.massMentionDenied).toBe(false);
  });

  test('Unicode word boundary: `@everyoneé` (Unicode letter trailing) does NOT match', () => {
    // EVERYONE_TOKEN_RE uses `\p{L}\p{N}_` (Unicode-aware) instead of
    // ASCII-only `[A-Za-z0-9_]` so non-ASCII letters following
    // `@everyone` don't break the word boundary. Pin that
    // `@everyoneé` falls through to the residue path (NOT mass-mention
    // expansion). ASCII-only boundary would have over-matched here.
    const int = makeInteraction({
      users: { '101': {}, '102': {} },
    });
    const res = parseRecipientMentions('@everyoneé', int, { allowMassMention: true });
    expect(res.ids).toEqual([]);
    expect(res.massMentionDenied).toBe(false);
    expect(res.invalidTokens).toEqual(['@​everyoneé']);
  });
});

describe('parseRecipientMentions — result-shape contract', () => {
  test('result shape pins exactly four keys (ids, invalidTokens, cappedCount, massMentionDenied)', () => {
    // Empire of `.toMatchObject` in other tests admits new fields by
    // design (the result shape is allowed to grow), but the closed
    // set of CURRENT keys is load-bearing — callers destructure these
    // names. Pin the closed set so a future PR that adds a 5th field
    // surfaces here intentionally rather than slipping past
    // partial-match assertions.
    const int = makeInteraction({ users: { '111': {} } });
    const res = parseRecipientMentions('<@111>', int);
    expect(Object.keys(res).sort()).toEqual([
      'cappedCount',
      'ids',
      'invalidTokens',
      'massMentionDenied',
    ]);
  });
});

describe('parseRecipientMentions — invalid tokens', () => {
  test('channel mentions land in invalidTokens', () => {
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('<@111> <#456>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['<#456>'], cappedCount: 0 });
  });

  test('custom emoji (static and animated) land in invalidTokens', () => {
    // Both `<:name:id>` (static) and `<a:name:id>` (animated) hit
    // the residue path — neither matches the USER_MENTION_RE
    // (`<@!?(\d+)>`) or ROLE_MENTION_RE (`<@&(\d+)>`) shapes.
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('<@111> <:smile:789>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['<:smile:789>'], cappedCount: 0 });
    expect(parseRecipientMentions('<@111> <a:dance:790>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['<a:dance:790>'], cappedCount: 0 });
  });

  test('bare plaintext usernames land in invalidTokens', () => {
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('alice <@111> bob', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['alice', 'bob'], cappedCount: 0 });
  });

  test('mention with missing closer (truncated input) is treated as invalid', () => {
    const int = makeInteraction({ users: { '111': {} } });
    const res = parseRecipientMentions('<@111 incomplete', int);
    // The truncated `<@111` cannot match the user-mention regex
    // (requires `>`), so it falls into the residual-token bucket.
    expect(res.ids).toEqual([]);
    expect(res.invalidTokens.length).toBeGreaterThan(0);
  });

  test('@everyone (denied) surfaces massMentionDenied + does NOT defuse into invalidTokens', () => {
    // With `allowMassMention` defaulting to false, `@everyone` is
    // recognized but the caller-side gate denies expansion. The
    // parser surfaces `massMentionDenied: true` so the caller can
    // emit a permission-specific warning, and strips the token from
    // input so the residue pass doesn't double-surface it as a
    // defused invalidToken. Distinct UX from "couldn't parse."
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('@everyone <@111>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: [], cappedCount: 0, massMentionDenied: true });
  });

  test('@here still defuses with zero-width-space (no presence intent, not implemented)', () => {
    // `@here` would need GUILD_PRESENCES intent to filter online
    // members — the bot only runs GuildMembers, so @here is left
    // on the legacy defuse path (rewritten with U+200B in
    // invalidTokens). Pin the invariant so a future PR that
    // implements @here flips this test deliberately, not silently.
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('@here <@111>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['@\u200bhere'], cappedCount: 0, massMentionDenied: false });
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
      .toMatchObject({ ids: ['111'], invalidTokens: ['@Everyone'], cappedCount: 0 });
    expect(parseRecipientMentions('@Here <@111>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['@Here'], cappedCount: 0 });
  });

  test('@here with trailing punctuation still defuses into invalidTokens', () => {
    // @here remains on the legacy defuse path until presence intent
    // lands — pin that trailing punctuation cases still get U+200B
    // protection regardless of @everyone's separate gated path.
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('@here: <@111>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['@\u200bhere:'], cappedCount: 0 });
  });

  test('@everyone with trailing punctuation (denied) still surfaces massMentionDenied + leftover lands in invalidTokens', () => {
    // `@everyone!`, `@everyone.fix`, etc. — the parser's
    // EVERYONE_TOKEN_RE matches `@everyone` with a non-word lookahead,
    // so trailing punctuation isn't consumed but the gate still fires.
    // Any leftover residue (`!`, `.fix`) is a tradeoff worth making —
    // surfacing the @everyone permission notice is the load-bearing
    // signal; the leftover punctuation gets a generic "couldn't
    // parse" treatment. Pin the residue shape so a future regression
    // that double-surfaces (`@everyone!` AS A WHOLE in invalidTokens
    // alongside the massMentionDenied flag) or silently swallows the
    // `!` is caught here.
    const int = makeInteraction({ users: { '111': {} } });
    const res = parseRecipientMentions('@everyone! <@111>', int);
    expect(res.ids).toEqual(['111']);
    expect(res.massMentionDenied).toBe(true);
    expect(res.invalidTokens).toEqual(['!']);
  });

  test('@everyone.fix (denied) leaves `.fix` in invalidTokens (parallel to `!` case)', () => {
    // `.fix` survives the strip-pass split class `[\s,;|/]+` — same
    // shape as `@everyone!`. Pin the residue so a future split-class
    // refactor that consumed `.` would surface here, not silently
    // change UX.
    const int = makeInteraction({ users: { '111': {} } });
    const res = parseRecipientMentions('@everyone.fix <@111>', int);
    expect(res.ids).toEqual(['111']);
    expect(res.massMentionDenied).toBe(true);
    expect(res.invalidTokens).toEqual(['.fix']);
  });

  test('here@everyone (embedded) stays on the defuse path (not a standalone @everyone)', () => {
    // Word-boundary semantics: `here@everyone` has `e` before the `@`,
    // so EVERYONE_TOKEN_RE's lookbehind rejects it. Falls into the
    // residue pass and gets U+200B-defused like before.
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('here@everyone <@111>', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['here@\u200beveryone'], cappedCount: 0, massMentionDenied: false });
  });

  test('newline characters separate tokens (split regex includes \\n)', () => {
    // Discord slash-command option strings can contain literal
    // newlines (paste-from-multi-line). Pin that the split regex
    // `/[\s,;|/]+/` includes \n — a regression that switched to
    // `/[ \t,;|/]+/` would silently merge "alice\nbob" into one
    // bare-name token.
    const int = makeInteraction({ users: { '111': {} } });
    expect(parseRecipientMentions('<@111>\n<#456>\n\nstray', int))
      .toMatchObject({ ids: ['111'], invalidTokens: ['<#456>', 'stray'], cappedCount: 0 });
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
      expect.objectContaining({ uniqueCount: 26, cap: 25, cappedCount: 1 }),
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
      expect.objectContaining({ uniqueCount: 51, cap: 25, cappedCount: 26 }),
    );
    expect(logger.debug).not.toHaveBeenCalled();
  });
});
